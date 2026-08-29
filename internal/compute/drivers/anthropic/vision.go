package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// Anthropic's vision shape: /v1/messages with an image content part.
//
// It lives beside the chat driver rather than in compute because the
// wire types are Anthropic's, and a vendor's shape belongs in the
// vendor's package — that separation is the whole reason the driver
// seam exists.
//
// Distinct from Chat despite the same endpoint family. An image
// question is one round trip with no tools, no streaming and no
// conversation; expressing it through the chat driver would mean
// building a ChatRequest that carried none of those and hoping every
// chat driver ignored them the same way.

// VisionFactory is the compute.VisionDriverFactory for Anthropic.
func VisionFactory(cfg compute.VisionDriverConfig) (compute.VisionDriver, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("anthropic vision: endpoint required")
	}
	if cfg.Model == "" {
		return nil, errors.New("anthropic vision: model required")
	}
	return &visionDriver{cfg: cfg, client: compute.HTTPClientOr(cfg.HTTPClient)}, nil
}

type visionDriver struct {
	cfg    compute.VisionDriverConfig
	client *http.Client
}

func (d *visionDriver) Describe(ctx context.Context, req compute.VisionRequest) (string, error) {
	// The image part comes FIRST. Anthropic's own guidance is that a
	// question after the image is answered more accurately than one
	// before it, and the old inline version got this right — worth
	// preserving through the move rather than rediscovering.
	body, err := json.Marshal(visionRequest{
		Model:     d.cfg.Model,
		MaxTokens: compute.VisionMaxTokens,
		Messages: []visionMessage{{
			Role: "user",
			Content: []contentPart{
				{Type: "image", Source: &imageSource{
					Type:      "base64",
					MediaType: req.MIME,
					Data:      base64.StdEncoding.EncodeToString(req.Data),
				}},
				{Type: "text", Text: req.Question},
			},
		}},
	})
	if err != nil {
		return "", err
	}
	// anthropic-version is required on every request and is NOT a
	// credential — it is the protocol version, and a driver that left
	// it to the wiring layer would work only for whoever remembered.
	// The chat driver sets it the same way, from the same constant.
	raw, err := compute.DoVisionRequest(ctx, d.client, d.cfg, http.MethodPost, d.cfg.Endpoint, body,
		compute.Header{Name: "anthropic-version", Value: apiVersion})
	if err != nil {
		return "", err
	}
	return decodeVision(raw)
}

type visionRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Messages  []visionMessage `json:"messages"`
}
type visionMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}
type contentPart struct {
	Type   string       `json:"type"`
	Text   string       `json:"text,omitempty"`
	Source *imageSource `json:"source,omitempty"`
}
type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// decodeVision concatenates the text parts.
//
// A reply may be several parts, and taking only the first would
// silently truncate a long description at whatever boundary the
// provider happened to choose.
func decodeVision(raw []byte) (string, error) {
	var decoded struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range decoded.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String(), nil
}
