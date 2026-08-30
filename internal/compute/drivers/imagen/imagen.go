// Package imagen implements the image modality against Vertex AI's
// Imagen models.
//
// Second image vendor, chosen for how little it shares with the
// first: the prompt is nested inside an instances array rather than
// sitting at the top level, knobs live in a sibling parameters object
// instead of beside the prompt, the operation is a :predict suffix on
// a model resource path rather than a fixed endpoint, and the
// response is predictions[] rather than data[].
//
// It also has no URL mode. Everything is inline base64, which is
// worth having behind the same interface as a driver that can return
// either — if ImageDriver only fit vendors offering both, it would be
// describing OpenAI rather than the modality.
package imagen

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

const DriverName = "imagen"

type Config struct {
	// Endpoint is the model resource base; ":predict" is appended.
	Endpoint   string
	Model      string
	Size       string
	Credential compute.Credential
	HTTPClient *http.Client
}

type Driver struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) (*Driver, error) {
	if cfg.Credential == nil {
		return nil, fmt.Errorf("imagen: credential required")
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("imagen: endpoint required (the model resource base URL)")
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 3 * time.Minute}
	}
	return &Driver{cfg: cfg, client: c}, nil
}

type instance struct {
	Prompt string `json:"prompt"`
}

type parameters struct {
	SampleCount int    `json:"sampleCount"`
	AspectRatio string `json:"aspectRatio,omitempty"`
}

type request struct {
	Instances  []instance `json:"instances"`
	Parameters parameters `json:"parameters"`
}

type response struct {
	Predictions []struct {
		BytesBase64 string `json:"bytesBase64Encoded"`
		MIMEType    string `json:"mimeType"`
		// Populated instead of an image when the safety filter blocks
		// the prompt, which is a 200 rather than a 4xx.
		RAIFilteredReason string `json:"raiFilteredReason"`
	} `json:"predictions"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (d *Driver) Generate(ctx context.Context, req compute.ImageRequest) (*compute.Artifact, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, compute.Permanent(fmt.Errorf("imagen: nothing to draw"))
	}

	body, err := json.Marshal(request{
		Instances: []instance{{Prompt: prompt}},
		Parameters: parameters{
			SampleCount: 1,
			AspectRatio: aspectRatio(cmp.Or(req.Size, d.cfg.Size)),
		},
	})
	if err != nil {
		return nil, compute.Permanent(fmt.Errorf("imagen: marshal: %w", err))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.Endpoint+":predict", bytes.NewReader(body))
	if err != nil {
		return nil, compute.Permanent(fmt.Errorf("imagen: build request: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := d.cfg.Credential.Apply(ctx, httpReq); err != nil {
		return nil, compute.Permanent(fmt.Errorf("imagen: apply credential: %w", err))
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, compute.Permanent(fmt.Errorf("imagen: %w", ctx.Err()))
		}
		return nil, compute.Transient(fmt.Errorf("imagen: http: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, compute.Transient(fmt.Errorf("imagen: read: %w", readErr))
	}
	if resp.StatusCode >= 400 {
		return nil, &compute.DriverError{
			Class: compute.ClassifyHTTPStatus(resp.StatusCode, string(raw)),
			Err:   fmt.Errorf("imagen: HTTP %d: %s", resp.StatusCode, truncate(raw)),
		}
	}

	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, compute.Permanent(fmt.Errorf("imagen: malformed response: %w", err))
	}
	if len(out.Predictions) == 0 {
		return nil, compute.Transient(fmt.Errorf("imagen: response carried no predictions"))
	}
	p := out.Predictions[0]

	// A blocked prompt arrives as a 200 with a reason and no image.
	// Permanent: the same prompt is refused every time, so walking the
	// failover chain would spend three providers' quota to be refused
	// three times.
	if p.RAIFilteredReason != "" {
		return nil, compute.Permanent(fmt.Errorf(
			"imagen: prompt blocked by the safety filter: %s", p.RAIFilteredReason))
	}
	if p.BytesBase64 == "" {
		return nil, compute.Transient(fmt.Errorf("imagen: prediction carried no image"))
	}
	decoded, err := base64.StdEncoding.DecodeString(p.BytesBase64)
	if err != nil {
		return nil, compute.Permanent(fmt.Errorf("imagen: decode image: %w", err))
	}

	mime := p.MIMEType
	if mime == "" {
		mime = "image/png"
	}
	return &compute.Artifact{Kind: compute.ArtifactInline, Bytes: decoded, MIME: mime}, nil
}

// aspectRatio translates the WxH sizes the tool exposes into the
// ratio strings this vendor takes. An operator who already knows the
// vendor vocabulary can pass a ratio straight through.
func aspectRatio(size string) string {
	s := strings.TrimSpace(size)
	if s == "" || strings.Contains(s, ":") {
		return s
	}
	w, h, ok := splitSize(s)
	if !ok {
		return ""
	}
	switch {
	case w == h:
		return "1:1"
	case w*9 == h*16:
		return "16:9"
	case w*16 == h*9:
		return "9:16"
	case w*3 == h*4:
		return "4:3"
	case w*4 == h*3:
		return "3:4"
	default:
		return ""
	}
}

func splitSize(s string) (w, h int, ok bool) {
	for _, sep := range []string{"x", "X", "*"} {
		if a, b, found := strings.Cut(s, sep); found {
			w, errW := strconv.Atoi(strings.TrimSpace(a))
			h, errH := strconv.Atoi(strings.TrimSpace(b))
			if errW == nil && errH == nil && w > 0 && h > 0 {
				return w, h, true
			}
		}
	}
	return 0, 0, false
}

func truncate(b []byte) string {
	const max = 512
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…[truncated]"
}
