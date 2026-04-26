package compute

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// VisionFormat picks the wire protocol for the read_image builtin.
type VisionFormat string

const (
	VisionFormatOpenAI    VisionFormat = "openai"
	VisionFormatAnthropic VisionFormat = "anthropic"
	VisionFormatGemini    VisionFormat = "gemini"
)

// VisionConfig wires the read_image builtin to a vision-capable
// endpoint. Empty Endpoint OR APIKey leaves the builtin
// unregistered — the agent will see no read_image tool and reply
// honestly that it can't view images.
type VisionConfig struct {
	Endpoint string
	Model    string
	APIKey   string
	// Format picks the wire protocol. Empty → openai.
	Format VisionFormat
	// AllowedRoot scopes which paths the agent can read. Empty →
	// "/workspace/incoming" (where the channel attachment downloader
	// drops files). Set to "" via SetAllowedRoot if you really want
	// to disable scoping (only sensible in tests).
	AllowedRoot string
	HTTPClient  *http.Client
}

// RegisterVisionBuiltin installs the read_image builtin. Returns
// an error on missing required fields so the operator gets a clear
// "not configured" message instead of a silent no-op.
func RegisterVisionBuiltin(b *Builtins, cfg VisionConfig) error {
	if cfg.Endpoint == "" || cfg.APIKey == "" {
		return errors.New("read_image: Endpoint and APIKey both required")
	}
	if cfg.Model == "" {
		return errors.New("read_image: Model required (e.g. \"abab6.5s-chat\", \"claude-opus-4\", \"gemini-2.0-flash\")")
	}
	if cfg.Format == "" {
		cfg.Format = VisionFormatOpenAI
	}
	switch cfg.Format {
	case VisionFormatOpenAI, VisionFormatAnthropic, VisionFormatGemini:
	default:
		return fmt.Errorf("read_image: unknown format %q (want openai|anthropic|gemini)", cfg.Format)
	}
	if cfg.AllowedRoot == "" {
		cfg.AllowedRoot = "/workspace/incoming"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return b.Register("read_image", newReadImageHandler(cfg, client))
}

// VisionToolDef is the ToolDef registered alongside the builtin.
// Description is deliberately direct — the agent must understand
// that this is the *only* path it has to "see" attachments when
// the main model is text-only.
func VisionToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name:        "read_image",
		Path:        BuiltinScheme + "read_image",
		Description: "View / understand an image at a local file path. The channel layer downloads inbound attachments to /workspace/incoming/<turn>/<file> and surfaces the path in the user's message via [user attached: ... path=...]. When the user attaches an image, ALWAYS call read_image with that path before answering — you have no other way to see it. Pass the optional question parameter to focus the description (e.g. 'is there a token plan visible?', 'transcribe any text'); leave empty for a general description. Returns the model's textual description as content.",
		ParametersSchema: []byte(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Absolute path to the image file (typically /workspace/incoming/<turn>/<file>)."},
				"question": {"type": "string", "description": "Optional focusing question. Empty → general description."}
			},
			"required": ["path"],
			"additionalProperties": false
		}`),
		RiskTier: types.RiskCommunicating,
	}
}

func newReadImageHandler(cfg VisionConfig, client *http.Client) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		path := strings.TrimSpace(args["path"])
		if path == "" {
			return nil, 2, errors.New("read_image: path is required")
		}
		question := strings.TrimSpace(args["question"])
		if question == "" {
			question = "Describe this image in detail. If it contains text, transcribe it accurately."
		}

		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, 2, fmt.Errorf("read_image: resolve path: %w", err)
		}
		if cfg.AllowedRoot != "" {
			rootAbs, err := filepath.Abs(cfg.AllowedRoot)
			if err != nil {
				return nil, 1, fmt.Errorf("read_image: resolve allowed root: %w", err)
			}
			if !strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) && abs != rootAbs {
				return nil, 2, fmt.Errorf("read_image: path %q outside allowed root %q", abs, rootAbs)
			}
		}

		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, 2, fmt.Errorf("read_image: read file: %w", err)
		}
		mime := sniffImageMime(abs, data)
		b64 := base64.StdEncoding.EncodeToString(data)

		var (
			body    []byte
			req     *http.Request
			content string
		)
		switch cfg.Format {
		case VisionFormatOpenAI:
			body, _ = json.Marshal(openAIVisionRequest{
				Model:     cfg.Model,
				MaxTokens: 1024,
				Messages: []openAIVisionMessage{{
					Role: "user",
					Content: []openAIVisionPart{
						{Type: "text", Text: question},
						{Type: "image_url", ImageURL: &openAIImageURL{URL: fmt.Sprintf("data:%s;base64,%s", mime, b64)}},
					},
				}},
			})
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(body))
			if err != nil {
				return nil, 1, err
			}
			req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

		case VisionFormatAnthropic:
			body, _ = json.Marshal(anthropicVisionRequest{
				Model:     cfg.Model,
				MaxTokens: 1024,
				Messages: []anthropicMessage{{
					Role: "user",
					Content: []anthropicContentPart{
						{Type: "image", Source: &anthropicImageSource{Type: "base64", MediaType: mime, Data: b64}},
						{Type: "text", Text: question},
					},
				}},
			})
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(body))
			if err != nil {
				return nil, 1, err
			}
			req.Header.Set("x-api-key", cfg.APIKey)
			req.Header.Set("anthropic-version", "2023-06-01")

		case VisionFormatGemini:
			body, _ = json.Marshal(geminiVisionRequest{
				Contents: []geminiContent{{
					Parts: []geminiPart{
						{Text: question},
						{InlineData: &geminiInlineData{MIMEType: mime, Data: b64}},
					},
				}},
			})
			endpoint := cfg.Endpoint
			if !strings.Contains(endpoint, "key=") {
				sep := "?"
				if strings.Contains(endpoint, "?") {
					sep = "&"
				}
				endpoint = endpoint + sep + "key=" + cfg.APIKey
			}
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
			if err != nil {
				return nil, 1, err
			}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, 1, fmt.Errorf("read_image: http: %w", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return nil, 1, fmt.Errorf("read_image: HTTP %d: %s", resp.StatusCode, truncateBodyFor(raw, 512))
		}

		switch cfg.Format {
		case VisionFormatOpenAI:
			content, err = decodeOpenAIVision(raw)
		case VisionFormatAnthropic:
			content, err = decodeAnthropicVision(raw)
		case VisionFormatGemini:
			content, err = decodeGeminiVision(raw)
		}
		if err != nil {
			return nil, 1, fmt.Errorf("read_image: decode (%s): %w", cfg.Format, err)
		}
		if content == "" {
			return nil, 1, fmt.Errorf("read_image: %s returned empty content", cfg.Format)
		}

		out, _ := json.Marshal(map[string]any{
			"path":    abs,
			"model":   cfg.Model,
			"format":  string(cfg.Format),
			"content": content,
		})
		return out, 0, nil
	}
}

// sniffImageMime picks a MIME type from the file extension first
// (cheap + accurate when present), falling back to byte-sniffing.
func sniffImageMime(path string, data []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	}
	if detected := http.DetectContentType(data); strings.HasPrefix(detected, "image/") {
		return detected
	}
	return "image/jpeg"
}

// --- OpenAI / MiniMax / OpenRouter shape ---

type openAIVisionRequest struct {
	Model     string                `json:"model"`
	MaxTokens int                   `json:"max_tokens,omitempty"`
	Messages  []openAIVisionMessage `json:"messages"`
}
type openAIVisionMessage struct {
	Role    string             `json:"role"`
	Content []openAIVisionPart `json:"content"`
}
type openAIVisionPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *openAIImageURL `json:"image_url,omitempty"`
}
type openAIImageURL struct {
	URL string `json:"url"`
}

func decodeOpenAIVision(raw []byte) (string, error) {
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", err
	}
	if len(decoded.Choices) == 0 {
		return "", errors.New("no choices")
	}
	return decoded.Choices[0].Message.Content, nil
}

// --- Anthropic /v1/messages shape ---

type anthropicVisionRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
}
type anthropicMessage struct {
	Role    string                 `json:"role"`
	Content []anthropicContentPart `json:"content"`
}
type anthropicContentPart struct {
	Type   string                `json:"type"`
	Text   string                `json:"text,omitempty"`
	Source *anthropicImageSource `json:"source,omitempty"`
}
type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

func decodeAnthropicVision(raw []byte) (string, error) {
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

// --- Gemini generateContent shape ---

type geminiVisionRequest struct {
	Contents []geminiContent `json:"contents"`
}
type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}
type geminiPart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inlineData,omitempty"`
}
type geminiInlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

func decodeGeminiVision(raw []byte) (string, error) {
	var decoded struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, cand := range decoded.Candidates {
		for _, p := range cand.Content.Parts {
			b.WriteString(p.Text)
		}
	}
	return b.String(), nil
}
