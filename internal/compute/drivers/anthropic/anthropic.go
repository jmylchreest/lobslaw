// Package anthropic implements the chat modality against Anthropic's
// Messages API.
//
// It is the second driver, and that is the point of it: one driver
// always fits its own interface. This one differs from the
// OpenAI-shaped client in every way that matters — the system prompt
// is a top-level field rather than a message, tool results are user
// messages carrying content blocks rather than a "tool" role, tool
// definitions use input_schema rather than a nested function object,
// auth is a bare x-api-key rather than a bearer token, and the API
// version travels in a header. If the waist can carry both without
// either leaking upward, it can carry a third.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// DefaultEndpoint is the Messages API.
const DefaultEndpoint = "https://api.anthropic.com/v1/messages"

// apiVersion is required on every request. Anthropic dates its
// breaking changes rather than versioning the path, so this is pinned
// deliberately: letting it float would let a vendor change alter our
// behaviour without a commit.
const apiVersion = "2023-06-01"

// defaultMaxTokens is required by the API — unlike OpenAI, max_tokens
// has no server-side default and the request is rejected without it.
const defaultMaxTokens = 4096

// normaliseEndpoint accepts a base URL as well as a full one.
//
// The openai driver has always taken either — LLMClient appends
// /chat/completions when it is absent, because that is the form every
// vendor's documentation quotes. This driver did not, so the same
// [[compute.providers]] field meant different things depending on the
// driver name beside it: endpoint = "https://api.anthropic.com" is
// what the Anthropic docs print, and it POSTed to the bare host.
//
// The failure was invisible until the first real turn, and then
// surfaced as "HTTP 404: " with an empty body — which reads as a
// broken vendor, not a config typo. One concept, one meaning.
func normaliseEndpoint(endpoint string) string {
	e := strings.TrimRight(endpoint, "/")
	switch {
	case strings.HasSuffix(e, "/messages"):
		return e
	case strings.HasSuffix(e, "/v1"):
		return e + "/messages"
	default:
		return e + "/v1/messages"
	}
}

// Config wires a driver instance.
type Config struct {
	Endpoint   string
	Model      string
	Credential compute.Credential
	HTTPClient *http.Client
	Logger     *slog.Logger
	// MaxTokens applies when a request does not set its own.
	MaxTokens int
}

// Driver is the Anthropic chat driver. Stateless per call.
type Driver struct {
	endpoint  string
	model     string
	cred      compute.Credential
	client    *http.Client
	log       *slog.Logger
	maxTokens int
}

// New builds a driver. A missing credential is a configuration bug
// rather than a runtime condition, so it fails here rather than on the
// first turn.
func New(cfg Config) (*Driver, error) {
	if cfg.Credential == nil {
		return nil, fmt.Errorf("anthropic: credential required")
	}
	d := &Driver{
		endpoint:  cfg.Endpoint,
		model:     cfg.Model,
		cred:      cfg.Credential,
		client:    cfg.HTTPClient,
		log:       cfg.Logger,
		maxTokens: cfg.MaxTokens,
	}
	if d.endpoint == "" {
		d.endpoint = DefaultEndpoint
	}
	d.endpoint = normaliseEndpoint(d.endpoint)
	if d.client == nil {
		d.client = &http.Client{Timeout: 120 * time.Second}
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	if d.maxTokens <= 0 {
		d.maxTokens = defaultMaxTokens
	}
	return d, nil
}

// Chat implements compute.ChatDriver.
func (d *Driver) Chat(ctx context.Context, req compute.ChatRequest) (*compute.ChatResponse, error) {
	model := req.Model
	if model == "" {
		model = d.model
	}
	wire, err := json.Marshal(d.toWire(req, model))
	if err != nil {
		return nil, compute.Permanent(fmt.Errorf("anthropic: marshal request: %w", err))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, bytes.NewReader(wire))
	if err != nil {
		return nil, compute.Permanent(fmt.Errorf("anthropic: build request: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("anthropic-version", apiVersion)
	if err := d.cred.Apply(ctx, httpReq); err != nil {
		return nil, compute.Permanent(fmt.Errorf("anthropic: apply credential: %w", err))
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		// A cancelled turn must surface as context.Canceled so callers
		// can tell "the user gave up" from "the provider is down".
		// http.Client wraps it in a *url.Error, which errors.Is sees
		// through, but the class must be set deliberately.
		if ctx.Err() != nil {
			return nil, compute.Permanent(fmt.Errorf("anthropic: %w", ctx.Err()))
		}
		return nil, compute.Transient(fmt.Errorf("anthropic: http do: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, compute.Transient(fmt.Errorf("anthropic: read body: %w", readErr))
	}

	if resp.StatusCode >= 400 {
		d.log.Warn("anthropic: response error",
			"status", resp.StatusCode, "model", model, "body", truncate(raw))
		return nil, &compute.DriverError{
			Class: compute.ClassifyHTTPStatus(resp.StatusCode, string(raw)),
			Err:   fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, truncate(raw)),
		}
	}

	var out wireResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, compute.Permanent(fmt.Errorf("anthropic: malformed response: %w (body: %s)", err, truncate(raw)))
	}
	return out.toChatResponse(), nil
}

// --- request translation --------------------------------------------

type wireRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    string        `json:"system,omitempty"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
	Temp      *float32      `json:"temperature,omitempty"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

// wireBlock is Anthropic's content-block union. One struct with
// omitempty rather than a real union, because the wire format is
// discriminated by "type" and Go's encoder handles that fine.
type wireBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// toWire translates lobslaw's OpenAI-shaped ChatRequest into the
// Messages API.
//
// Three structural differences, each of which would leak upward if the
// waist were shaped around OpenAI:
//
//   - the system prompt is a top-level field, not a message;
//   - a tool result is a USER message containing a tool_result block,
//     not a message with role "tool";
//   - consecutive messages of the same role are rejected, so tool
//     results are merged into one user message.
func (d *Driver) toWire(req compute.ChatRequest, model string) wireRequest {
	out := wireRequest{Model: model, MaxTokens: req.MaxTokens}
	if out.MaxTokens <= 0 {
		out.MaxTokens = d.maxTokens
	}
	if req.Temperature > 0 {
		t := req.Temperature
		out.Temp = &t
	}

	for _, t := range req.Tools {
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, wireTool{
			Name: t.Name, Description: t.Description, InputSchema: schema,
		})
	}

	var pendingToolResults []wireBlock
	flush := func() {
		if len(pendingToolResults) > 0 {
			out.Messages = append(out.Messages, wireMessage{Role: "user", Content: pendingToolResults})
			pendingToolResults = nil
		}
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			// Concatenated rather than overwritten: promptgen may emit
			// more than one system message, and dropping all but the
			// last would silently discard instructions.
			if out.System != "" {
				out.System += "\n\n"
			}
			out.System += m.Content

		case "tool":
			pendingToolResults = append(pendingToolResults, wireBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
			})

		case "assistant":
			flush()
			blocks := make([]wireBlock, 0, 1+len(m.ToolCalls))
			if m.Content != "" {
				blocks = append(blocks, wireBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				args := json.RawMessage(tc.Arguments)
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				blocks = append(blocks, wireBlock{
					Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: args,
				})
			}
			if len(blocks) == 0 {
				continue
			}
			out.Messages = append(out.Messages, wireMessage{Role: "assistant", Content: blocks})

		default: // user
			flush()
			out.Messages = append(out.Messages, wireMessage{
				Role: "user", Content: []wireBlock{{Type: "text", Text: m.Content}},
			})
		}
	}
	flush()
	return out
}

// --- response translation -------------------------------------------

type wireResponse struct {
	Content    []wireBlock `json:"content"`
	StopReason string      `json:"stop_reason"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func (r *wireResponse) toChatResponse() *compute.ChatResponse {
	out := &compute.ChatResponse{FinishReason: normaliseStop(r.StopReason)}
	var text strings.Builder
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, compute.ToolCall{
				ID: b.ID, Name: b.Name, Arguments: string(b.Input),
			})
		}
	}
	out.Content = text.String()

	// Cache reads are billed differently from fresh input, and
	// Anthropic reports them separately rather than inside input
	// tokens. Folding them in would overstate the cost of exactly the
	// long-context turns caching exists to make cheap.
	in := r.Usage.InputTokens + r.Usage.CacheCreationInputTokens
	out.Usage = compute.Usage{
		PromptTokens:     in + r.Usage.CacheReadInputTokens,
		CompletionTokens: r.Usage.OutputTokens,
		TotalTokens:      in + r.Usage.CacheReadInputTokens + r.Usage.OutputTokens,
		CachedTokens:     r.Usage.CacheReadInputTokens,
	}
	return out
}

// normaliseStop maps Anthropic's stop reasons onto the OpenAI-shaped
// vocabulary the agent loop already branches on. Keeping the
// translation here is the point of a driver: the loop should not learn
// a second vocabulary for every vendor.
func normaliseStop(s string) string {
	switch s {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "end_turn", "stop_sequence":
		return "stop"
	case "":
		return "stop"
	default:
		// Anthropic adds stop reasons over time — "refusal" is the
		// current example — so unknown values pass through rather than
		// being flattened to "stop". That is safe because the agent
		// loop continues on tool calls being PRESENT, not on the
		// finish reason, and only logs this: an unrecognised reason
		// ends the turn and reaches the operator intact, where
		// flattening it would hide a refusal as a normal answer.
		return s
	}
}

func truncate(b []byte) string {
	const max = 512
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…[truncated]"
}
