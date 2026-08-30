package compute

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ImageRequest is one text-to-image call.
type ImageRequest struct {
	Model  string
	Prompt string

	// Size is vendor-specific ("1024x1024", "1792x1024"). Empty picks
	// the driver's configured default.
	Size string

	// Quality is vendor-specific ("standard", "hd", "low"). Empty
	// leaves it to the provider.
	Quality string
}

// ImageDriver renders a prompt as a picture.
type ImageDriver interface {
	Generate(ctx context.Context, req ImageRequest) (*Artifact, error)
}

// OpenAIImageConfig wires the /v1/images/generations shape.
type OpenAIImageConfig struct {
	Endpoint   string
	Model      string
	Size       string
	Quality    string
	Credential Credential
	HTTPClient *http.Client

	// PreferURL asks for a hosted URL instead of inline base64 where
	// the provider offers the choice. Inline is the default because it
	// cannot expire between generation and delivery; a URL is worth it
	// for large images, and the resolver downloads either.
	PreferURL bool
}

// OpenAIImageDriver speaks /v1/images/generations — OpenAI's, and the
// shape most hosted and self-hosted image services imitate.
type OpenAIImageDriver struct {
	cfg    OpenAIImageConfig
	client *http.Client
}

func NewOpenAIImageDriver(cfg OpenAIImageConfig) (*OpenAIImageDriver, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("image: endpoint required")
	}
	if cfg.Credential == nil {
		return nil, fmt.Errorf("image: credential required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("image: model required")
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 3 * time.Minute}
	}
	return &OpenAIImageDriver{cfg: cfg, client: c}, nil
}

type imageWire struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size,omitempty"`
	Quality        string `json:"quality,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}

type imageWireResponse struct {
	Data []struct {
		B64  string `json:"b64_json"`
		URL  string `json:"url"`
		MIME string `json:"output_format"`
	} `json:"data"`
}

func (d *OpenAIImageDriver) Generate(ctx context.Context, req ImageRequest) (*Artifact, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, Permanent(fmt.Errorf("image: nothing to draw"))
	}

	format := "b64_json"
	if d.cfg.PreferURL {
		format = "url"
	}
	body, err := json.Marshal(imageWire{
		Model:          cmp.Or(req.Model, d.cfg.Model),
		Prompt:         prompt,
		N:              1,
		Size:           cmp.Or(req.Size, d.cfg.Size),
		Quality:        cmp.Or(req.Quality, d.cfg.Quality),
		ResponseFormat: format,
	})
	if err != nil {
		return nil, Permanent(fmt.Errorf("image: marshal: %w", err))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, Permanent(fmt.Errorf("image: build request: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := d.cfg.Credential.Apply(ctx, httpReq); err != nil {
		return nil, Permanent(fmt.Errorf("image: apply credential: %w", err))
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, Permanent(fmt.Errorf("image: %w", ctx.Err()))
		}
		return nil, Transient(fmt.Errorf("image: http: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, Transient(fmt.Errorf("image: read: %w", readErr))
	}
	if resp.StatusCode >= 400 {
		return nil, &DriverError{
			Class: ClassifyHTTPStatus(resp.StatusCode, string(raw)),
			Err:   fmt.Errorf("image: HTTP %d: %s", resp.StatusCode, truncateBodyFor(raw, 512)),
		}
	}

	var out imageWireResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, Permanent(fmt.Errorf("image: malformed response: %w", err))
	}
	if len(out.Data) == 0 {
		return nil, Transient(fmt.Errorf("image: provider returned no images"))
	}
	first := out.Data[0]

	mime := "image/png"
	if first.MIME != "" {
		mime = "image/" + strings.TrimPrefix(first.MIME, "image/")
	}

	// Whichever delivery mode the provider chose, the resolver
	// normalises it. A URL here is short-lived at most vendors, which
	// is why inline is the default.
	if first.URL != "" {
		return &Artifact{Kind: ArtifactURL, URL: first.URL, MIME: mime}, nil
	}
	if first.B64 == "" {
		return nil, Transient(fmt.Errorf("image: response carried neither url nor b64_json"))
	}
	decoded, err := base64.StdEncoding.DecodeString(first.B64)
	if err != nil {
		return nil, Permanent(fmt.Errorf("image: decode base64: %w", err))
	}
	return &Artifact{Kind: ArtifactInline, Bytes: decoded, MIME: mime}, nil
}
