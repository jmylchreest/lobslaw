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

// PDFConfig wires the read_pdf builtin. Today there's only one wire
// shape: OpenRouter's chat-completions with a `file` content part
// carrying base64-encoded PDF bytes (the same shape as their
// input_audio for audio). Other providers exposing PDF reading
// (Anthropic native PDF, Gemini PDF) can land here later as
// PDFFormat constants in the same dispatch pattern as audio/vision.
type PDFConfig struct {
	Endpoint    string
	Model       string
	APIKey      string
	AllowedRoot string
	HTTPClient  *http.Client
}

// RegisterPDFBuiltin installs read_pdf.
func RegisterPDFBuiltin(b *Builtins, cfg PDFConfig) error {
	if cfg.Endpoint == "" || cfg.APIKey == "" {
		return errors.New("read_pdf: Endpoint and APIKey both required")
	}
	if cfg.Model == "" {
		return errors.New("read_pdf: Model required (e.g. \"google/gemini-2.0-flash-001\", \"anthropic/claude-opus-4\")")
	}
	if cfg.AllowedRoot == "" {
		cfg.AllowedRoot = "/workspace/incoming"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	return b.Register("read_pdf", newReadPDFHandler(cfg, client))
}

func PDFToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name:        "read_pdf",
		Path:        BuiltinScheme + "read_pdf",
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

func newReadPDFHandler(cfg PDFConfig, client *http.Client) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		path := strings.TrimSpace(args["path"])
		if path == "" {
			return nil, 2, errors.New("read_pdf: path is required")
		}
		question := strings.TrimSpace(args["question"])
		if question == "" {
			question = "Read this PDF and provide a structured summary. Note any dates, names, key figures, and the document's purpose. Transcribe text verbatim where the content is short."
		}

		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, 2, fmt.Errorf("read_pdf: resolve path: %w", err)
		}
		if cfg.AllowedRoot != "" {
			rootAbs, err := filepath.Abs(cfg.AllowedRoot)
			if err != nil {
				return nil, 1, fmt.Errorf("read_pdf: resolve allowed root: %w", err)
			}
			if !strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) && abs != rootAbs {
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
			return nil, 1, fmt.Errorf("read_pdf: http: %w", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return nil, 1, fmt.Errorf("read_pdf: HTTP %d: %s", resp.StatusCode, truncateBodyFor(raw, 512))
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
