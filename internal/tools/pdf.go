package tools

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

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// defaultPDFQuestion is asked when the caller focuses on nothing in
// particular. It names dates, figures and purpose explicitly: a model
// asked only to "summarise" a document reliably drops the details
// somebody is about to be asked about.
const defaultPDFQuestion = "Read this PDF and provide a structured summary. Note any dates, names, key figures, and the document's purpose. Transcribe text verbatim where the content is short."

// PDFConfig wires the read_pdf builtin. Today there's only one wire
// shape: OpenRouter's chat-completions with a `file` content part
// carrying base64-encoded PDF bytes (the same shape as their
// input_audio for audio). Other providers exposing PDF reading
// (Anthropic native PDF, Gemini PDF) land as PDF DRIVERS registered in
// the DriverSet — not as constants in a switch. Audio and vision both
// grew that switch and both have had it taken out again: it puts the
// vendor's shape in the builtin, and the builtin's job is the path
// check and the sniff.
type PDFConfig struct {
	Endpoint    string
	Model       string
	APIKey      string
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

// RegisterPDFBuiltin installs read_pdf.
// Variadic for the same reason as vision and audio: one config is a
// single provider, several are a failover chain in the order given.
func RegisterPDFBuiltin(b *Builtins, cfgs ...PDFConfig) error {
	if len(cfgs) == 0 {
		return errors.New("read_pdf: at least one provider config required")
	}
	handlers := make([]compute.FailoverHandler, 0, len(cfgs))
	for _, cfg := range cfgs {
		if cfg.Endpoint == "" || cfg.APIKey == "" {
			return errors.New("read_pdf: Endpoint and APIKey both required")
		}
		if cfg.Model == "" {
			return errors.New("read_pdf: Model required (e.g. \"google/gemini-2.0-flash-001\", \"anthropic/claude-opus-4\")")
		}
		// Inherited from a provider entry, whose endpoint is a base
		// URL. Used verbatim this posts to the base and 404s.
		cfg.Endpoint = compute.NormaliseEndpoint(cfg.Endpoint, compute.ChatCompletionsPath)
		if cfg.AllowedRoot == "" {
			cfg.AllowedRoot = DefaultIncomingDir
		}
		client := cfg.HTTPClient
		if client == nil {
			client = &http.Client{Timeout: 120 * time.Second}
		}
		handlers = append(handlers, compute.FailoverHandler{Label: cfg.Label, Tier: cfg.TrustTier, Fn: newReadPDFHandler(cfg, client)})
	}
	return b.Register("read_pdf", compute.FailoverBuiltin("read_pdf", nil, b.Health(), b.TrustFloor(), handlers...))
}

func PDFToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name:        "read_pdf",
		Path:        compute.BuiltinScheme + "read_pdf",
		Description: "Read / extract content from a PDF file at a local path. Channel layer downloads inbound document attachments to /workspace/incoming/<turn>/<file>. When the user attaches a PDF, ALWAYS call read_pdf with that path before answering. Pass the optional question parameter to focus the extraction (e.g. 'extract all the dates', 'summarise the conclusions'); leave empty for a structured summary. Returns the model's textual extraction as content.",
		ParametersSchema: []byte(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Absolute path to the PDF file (typically /workspace/incoming/<turn>/<file>)."},
				"question": {"type": "string", "description": "Optional focusing question. Empty → structured summary."}
			},
			"required": ["path"],
			"additionalProperties": false
		}`),
		RiskTier: types.RiskCommunicating,
	}
}

func newReadPDFHandler(cfg PDFConfig, client *http.Client) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		path := strings.TrimSpace(args["path"])
		if path == "" {
			return nil, 2, errors.New("read_pdf: path is required")
		}
		question := strings.TrimSpace(args["question"])
		if question == "" {
			question = defaultPDFQuestion
		}

		// The shared chain: mounts, absolute, cluster-internal,
		// hardline, then the operator's policy.d. This used to be an
		// AllowedRoot prefix test and nothing else — which meant a
		// modality tool could read a TLS key that read_file refuses,
		// as long as somebody had pointed a root at it.
		abs, payload, exit := guardReadWithin("read_pdf", path, cfg.AllowedRoot)
		if exit != 0 {
			return payload, exit, nil
		}
		// AllowedRoot survives as a FURTHER narrowing. It is how the
		// channel layer pins these tools to the directory it drops
		// inbound attachments in, which is tighter than any mount.
		if cfg.AllowedRoot != "" {
			rootAbs, err := filepath.Abs(cfg.AllowedRoot)
			if err != nil {
				return nil, 1, fmt.Errorf("read_pdf: resolve allowed root: %w", err)
			}
			if !pathWithinRoot(abs, rootAbs) {
				return nil, 2, fmt.Errorf("read_pdf: path %q outside allowed root %q", abs, rootAbs)
			}
		}

		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, 2, fmt.Errorf("read_pdf: read file: %w", err)
		}
		b64 := base64.StdEncoding.EncodeToString(data)
		filename := filepath.Base(abs)

		body, _ := json.Marshal(pdfChatRequest{
			Model:     cfg.Model,
			MaxTokens: 4096,
			Messages: []pdfChatMessage{{
				Role: "user",
				Content: []pdfChatPart{
					{Type: "text", Text: question},
					{Type: "file", File: &pdfFile{Filename: filename, FileData: "data:application/pdf;base64," + b64}},
				},
			}},
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, 1, err
		}
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, 1, compute.Transient(fmt.Errorf("read_pdf: http: %w", err))
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return nil, 1, &compute.DriverError{
				Class: compute.ClassifyHTTPStatus(resp.StatusCode, string(raw)),
				Err:   fmt.Errorf("read_pdf: HTTP %d: %s", resp.StatusCode, compute.TruncateBodyFor(raw, 512)),
			}
		}
		var decoded struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, 1, fmt.Errorf("read_pdf: decode: %w", err)
		}
		if len(decoded.Choices) == 0 {
			return nil, 1, errors.New("read_pdf: provider returned no choices")
		}
		out, _ := json.Marshal(map[string]any{
			"path":    abs,
			"model":   cfg.Model,
			"content": decoded.Choices[0].Message.Content,
		})
		return out, 0, nil
	}
}

type pdfChatRequest struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens,omitempty"`
	Messages  []pdfChatMessage `json:"messages"`
}
type pdfChatMessage struct {
	Role    string        `json:"role"`
	Content []pdfChatPart `json:"content"`
}
type pdfChatPart struct {
	Type string   `json:"type"`
	Text string   `json:"text,omitempty"`
	File *pdfFile `json:"file,omitempty"`
}
type pdfFile struct {
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
}
