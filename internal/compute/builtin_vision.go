package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// DefaultIncomingDir is where channel handlers materialise inbound
// attachments, and the only directory the read_* builtins will open a
// path in when no root is configured.
//
// Defined here rather than in gateway because gateway imports compute
// and the reverse would cycle — and because the writer and the reader
// of this directory must never hold two different opinions about
// where it is.
//
// Only the CONTAINER image has a /workspace. On a host install
// nothing creates it, MkdirAll under it fails for an unprivileged
// process, and the result was that an inbound photograph could not be
// received AND could not be read. See [gateway] incoming_dir.
const DefaultIncomingDir = "/workspace/incoming"

// VisionConfig wires the read_image builtin to a vision-capable
// endpoint. Empty Endpoint OR APIKey leaves the builtin
// unregistered — the agent will see no read_image tool and reply
// honestly that it can't view images.
type VisionConfig struct {
	Endpoint string
	Model    string
	APIKey   string
	// Driver is the resolved wire protocol for this endpoint. A named
	// driver rather than a format enum, so adding a vendor is one
	// registration and not an edit to every switch that decodes a
	// reply.
	Driver VisionDriver
	// AllowedRoot scopes which paths the agent can read. Empty →
	// DefaultIncomingDir (where the channel attachment downloader
	// drops files). Set to "" via SetAllowedRoot if you really want
	// to disable scoping (only sensible in tests).
	AllowedRoot string
	HTTPClient  *http.Client

	// Label is the provider's config label, used as the health key so
	// a demotion is shared with every other modality that reaches the
	// same endpoint. Empty opts out of health tracking.
	Label string

	// TrustTier is the provider's declared tier, checked against the
	// soul's min_trust_tier before this provider is used. Empty fails
	// any set floor: an undeclared tier is not evidence of a high one.
	TrustTier types.TrustTier
}

// RegisterVisionBuiltin installs the read_image builtin. Returns
// an error on missing required fields so the operator gets a clear
// "not configured" message instead of a silent no-op.
// Variadic: one config is a single provider, several are a failover
// chain tried in the order given. Existing single-config callers are
// unaffected, and a chain shares one validation path — a
// misconfigured backup fails at boot rather than at 3am when the
// primary finally goes down.
func RegisterVisionBuiltin(b *Builtins, cfgs ...VisionConfig) error {
	if len(cfgs) == 0 {
		return errors.New("read_image: at least one provider config required")
	}
	handlers := make([]failoverHandler, 0, len(cfgs))
	for _, cfg := range cfgs {
		if cfg.Endpoint == "" || cfg.APIKey == "" {
			return errors.New("read_image: Endpoint and APIKey both required")
		}
		if cfg.Model == "" {
			return errors.New("read_image: Model required (e.g. \"abab6.5s-chat\", \"claude-opus-4\", \"gemini-2.0-flash\")")
		}

		if cfg.Driver == nil {
			return errors.New("read_image: Driver required (resolve it from the DriverSet)")
		}
		if cfg.AllowedRoot == "" {
			cfg.AllowedRoot = DefaultIncomingDir
		}
		client := cfg.HTTPClient
		if client == nil {
			client = &http.Client{Timeout: 60 * time.Second}
		}
		handlers = append(handlers, failoverHandler{label: cfg.Label, tier: cfg.TrustTier, fn: newReadImageHandler(cfg, client)})
	}
	return b.Register("read_image", failoverBuiltin("read_image", nil, b.Health(), b.TrustFloor(), handlers...))
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

		// The shared chain: mounts, absolute, cluster-internal,
		// hardline, then the operator's policy.d. This used to be an
		// AllowedRoot prefix test and nothing else — which meant a
		// modality tool could read a TLS key that read_file refuses,
		// as long as somebody had pointed a root at it.
		abs, payload, exit := guardReadWithin("read_image", path, cfg.AllowedRoot)
		if exit != 0 {
			return payload, exit, nil
		}
		// AllowedRoot survives as a FURTHER narrowing. It is how the
		// channel layer pins these tools to the directory it drops
		// inbound attachments in, which is tighter than any mount.
		if cfg.AllowedRoot != "" {
			rootAbs, err := filepath.Abs(cfg.AllowedRoot)
			if err != nil {
				return nil, 1, fmt.Errorf("read_image: resolve allowed root: %w", err)
			}
			if !pathWithinRoot(abs, rootAbs) {
				return nil, 2, fmt.Errorf("read_image: path %q outside allowed root %q", abs, rootAbs)
			}
		}

		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, 2, fmt.Errorf("read_image: read file: %w", err)
		}
		mime := sniffImageMime(abs, data)

		// The builtin owns the path check and the sniff; the driver
		// owns the wire. That line is what makes a new vendor one file
		// rather than an edit to every switch in here.
		content, err := cfg.Driver.Describe(ctx, VisionRequest{
			Question: question,
			MIME:     mime,
			Data:     data,
		})
		if err != nil {
			return nil, 1, err
		}
		if content == "" {
			return nil, 1, errors.New("read_image: the provider returned empty content")
		}

		out, _ := json.Marshal(map[string]any{
			"path":    abs,
			"model":   cfg.Model,
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
