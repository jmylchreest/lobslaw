package dashscope

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
)

// DefaultImageEndpoint is the synchronous multimodal-generation path.
//
// NOT the text2image/image-synthesis path, which is the one the video
// side of this package uses and the one most DashScope documentation
// leads with. That path is asynchronous — it needs X-DashScope-Async
// and returns a task to poll — and a token plan that does not permit
// asynchronous calls answers it with
//
//	403 AccessDenied: current user api does not support asynchronous calls
//
// The multimodal-generation path renders inline and returns a URL, so
// it works on plans where the async one does not. The Wan and
// Qwen-Image models are reachable through it.
const DefaultImageEndpoint = "https://dashscope-intl.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation"

// ImageConfig wires one driver instance.
type ImageConfig struct {
	Endpoint   string
	Model      string
	Credential compute.Credential
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// ImageDriver renders prompts through DashScope.
type ImageDriver struct {
	cfg    ImageConfig
	client *http.Client
}

// NewImage builds a driver.
func NewImage(cfg ImageConfig) (*ImageDriver, error) {
	if cfg.Credential == nil {
		return nil, fmt.Errorf("dashscope image: credential required")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultImageEndpoint
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &ImageDriver{cfg: cfg, client: compute.HTTPClientOr(cfg.HTTPClient)}, nil
}

// The request is a chat-shaped envelope even though nothing about
// this is a conversation: input.messages[].content[].text. Sending
// {input:{prompt:"..."}} instead returns "url error, please check
// url", which names neither the field nor the problem.
type imageRequest struct {
	Model string `json:"model"`
	Input struct {
		Messages []imageMessage `json:"messages"`
	} `json:"input"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

type imageMessage struct {
	Role    string             `json:"role"`
	Content []imageContentPart `json:"content"`
}

type imageContentPart struct {
	Text  string `json:"text,omitempty"`
	Type  string `json:"type,omitempty"`
	Image string `json:"image,omitempty"`
}

type imageResponse struct {
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Output    struct {
		Choices []struct {
			Message struct {
				Content []imageContentPart `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	} `json:"output"`
}

// Generate renders one image and returns it as a URL artifact.
func (d *ImageDriver) Generate(ctx context.Context, req compute.ImageRequest) (*compute.Artifact, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, compute.Permanent(fmt.Errorf("dashscope image: prompt required"))
	}
	model := req.Model
	if model == "" {
		model = d.cfg.Model
	}

	var wire imageRequest
	wire.Model = model
	wire.Input.Messages = []imageMessage{{
		Role:    "user",
		Content: []imageContentPart{{Text: prompt}},
	}}
	if size := dashscopeSize(req.Size); size != "" {
		wire.Parameters = map[string]any{"size": size}
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, compute.Permanent(fmt.Errorf("dashscope image: marshal: %w", err))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, compute.Permanent(fmt.Errorf("dashscope image: build request: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// No X-DashScope-Async header, deliberately. See DefaultImageEndpoint.
	if err := d.cfg.Credential.Apply(ctx, httpReq); err != nil {
		return nil, compute.Permanent(fmt.Errorf("dashscope image: apply credential: %w", err))
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, compute.Permanent(fmt.Errorf("dashscope image: %w", ctx.Err()))
		}
		return nil, compute.Transient(fmt.Errorf("dashscope image: http: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, compute.Transient(fmt.Errorf("dashscope image: read: %w", readErr))
	}
	if resp.StatusCode >= 400 {
		return nil, &compute.DriverError{
			Class: compute.ClassifyHTTPStatus(resp.StatusCode, string(raw)),
			Err: fmt.Errorf("dashscope image: HTTP %d: %s",
				resp.StatusCode, truncate(raw)),
		}
	}

	var out imageResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, compute.Permanent(fmt.Errorf("dashscope image: malformed response: %w", err))
	}
	// A code on a 2xx is still a failure. Carried through with the
	// vendor's own wording, which is the only part of it that helps.
	if out.Code != "" {
		return nil, compute.Permanent(fmt.Errorf("dashscope image: %s: %s", out.Code, out.Message))
	}
	url := firstImageURL(out)
	if url == "" {
		return nil, compute.Transient(fmt.Errorf("dashscope image: response carried no image"))
	}
	// URLs here expire (they carry an Expires parameter), so the
	// artifact resolver downloads before they go stale.
	return &compute.Artifact{Kind: compute.ArtifactURL, URL: url, MIME: "image/png"}, nil
}

// firstImageURL digs the URL out of the chat-shaped envelope.
//
// The parts are typed, and a text part sits alongside an image part
// when the model narrates what it drew. Taking content[0] blindly
// would return prose as a picture on exactly those replies.
func firstImageURL(out imageResponse) string {
	for _, c := range out.Output.Choices {
		for _, part := range c.Message.Content {
			if part.Image != "" {
				return part.Image
			}
		}
	}
	return ""
}

// dashscopeSize converts the "1024x1024" spelling every other vendor
// uses into the "1024*1024" this one requires.
//
// Passing it through unchanged is an HTTP 400 —
//
//	Invalid size format: 1024x1024, expected format: width*height
//
// — on every request carrying a size, which is a failure the operator
// cannot fix from config because the size comes from the tool call.
// Anything not matching WIDTHxHEIGHT is passed through: a vendor
// keyword this driver has not heard of is the caller's business, and
// mangling it would be worse than forwarding it.
func dashscopeSize(size string) string {
	if size == "" {
		return ""
	}
	w, h, ok := strings.Cut(size, "x")
	if !ok || !allDigits(w) || !allDigits(h) {
		return size
	}
	return w + "*" + h
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
