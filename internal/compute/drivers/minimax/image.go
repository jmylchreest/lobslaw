// Package minimax implements MiniMax's generation APIs.
//
// Chat is OpenAI-compatible and needs no driver of its own; image
// generation is not, which is the case the driver seam exists for —
// "a custom variant is a different endpoint" holds for chat and does
// not hold for generation.
package minimax

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/textutil"
)

// DriverName is what config names in `driver = "..."`.
const DriverName = "minimax"

// DefaultImageEndpoint is the image-generation path.
const DefaultImageEndpoint = "https://api.minimax.io/v1/image_generation"

// ImageConfig wires one driver instance.
type ImageConfig struct {
	Endpoint   string
	Model      string
	Credential compute.Credential
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// ImageDriver renders prompts through MiniMax.
type ImageDriver struct {
	cfg    ImageConfig
	client *http.Client
}

// NewImage builds a driver. A missing credential is a configuration
// bug rather than a runtime condition, so it fails here rather than on
// the first picture somebody asks for.
func NewImage(cfg ImageConfig) (*ImageDriver, error) {
	if cfg.Credential == nil {
		return nil, fmt.Errorf("minimax image: credential required")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultImageEndpoint
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &ImageDriver{cfg: cfg, client: compute.HTTPClientOr(cfg.HTTPClient)}, nil
}

type imageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	ResponseFormat string `json:"response_format,omitempty"`
}

type imageResponse struct {
	Data *struct {
		ImageURLs []string `json:"image_urls"`
	} `json:"data"`
	BaseResp baseResp `json:"base_resp"`
}

// baseResp is MiniMax's in-body status.
//
// It matters more here than the HTTP code does: an unsupported model
// comes back as HTTP 200 with data:null and status_code 2013. A driver
// that trusted the status line would report "provider returned no
// images" and throw away the one sentence saying why.
type baseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

// Generate renders one image and returns it as a URL artifact.
func (d *ImageDriver) Generate(ctx context.Context, req compute.ImageRequest) (*compute.Artifact, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, compute.Permanent(fmt.Errorf("minimax image: prompt required"))
	}
	model := req.Model
	if model == "" {
		model = d.cfg.Model
	}
	body, err := json.Marshal(imageRequest{
		Model: model, Prompt: prompt, N: 1, ResponseFormat: "url",
	})
	if err != nil {
		return nil, compute.Permanent(fmt.Errorf("minimax image: marshal: %w", err))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, compute.Permanent(fmt.Errorf("minimax image: build request: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := d.cfg.Credential.Apply(ctx, httpReq); err != nil {
		return nil, compute.Permanent(fmt.Errorf("minimax image: apply credential: %w", err))
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, compute.Permanent(fmt.Errorf("minimax image: %w", ctx.Err()))
		}
		return nil, compute.Transient(fmt.Errorf("minimax image: http: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, compute.Transient(fmt.Errorf("minimax image: read: %w", readErr))
	}
	if resp.StatusCode >= 400 {
		return nil, &compute.DriverError{
			Class: compute.ClassifyHTTPStatus(resp.StatusCode, string(raw)),
			Err:   fmt.Errorf("minimax image: HTTP %d: %s", resp.StatusCode, textutil.Truncate(string(raw), "…[truncated]", 512)),
		}
	}

	var out imageResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, compute.Permanent(fmt.Errorf("minimax image: malformed response: %w", err))
	}
	// Checked BEFORE the payload, because a failure arrives as HTTP
	// 200 with data:null — reading the payload first turns "unsupported
	// model" into "no images returned".
	if out.BaseResp.StatusCode != 0 {
		return nil, classifyStatus(out.BaseResp)
	}
	if out.Data == nil || len(out.Data.ImageURLs) == 0 {
		return nil, compute.Transient(fmt.Errorf("minimax image: response carried no image urls"))
	}
	// The URL is short-lived (it carries an Expires parameter); the
	// artifact resolver downloads it before it goes stale.
	return &compute.Artifact{
		Kind: compute.ArtifactURL,
		URL:  out.Data.ImageURLs[0],
		MIME: "image/jpeg",
	}, nil
}

// classifyStatus decides whether an in-body failure is worth retrying.
//
// Rate limits and internal errors are; an unsupported model or a
// rejected prompt is not, and retrying one burns quota to be told the
// same thing again.
func classifyStatus(b baseResp) error {
	err := fmt.Errorf("minimax image: status %d: %s", b.StatusCode, b.StatusMsg)
	switch b.StatusCode {
	case 1002, 1039: // rate limited / concurrency limited
		return compute.Transient(err)
	case 1000, 1013: // unknown + internal error
		return compute.Transient(err)
	default:
		return compute.Permanent(err)
	}
}
