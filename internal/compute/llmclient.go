package compute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

// DefaultLLMTimeout is the HTTP deadline the client applies to each
// Chat() round-trip when the caller's context doesn't already carry
// a shorter one. Generous — LLM calls with long outputs can legitimately
// take 60+ seconds at the provider side.
const DefaultLLMTimeout = 120 * time.Second

// Sentinel errors for the OpenAI-compatible client. Callers wrap
// with fmt.Errorf where more context is useful; tests / the agent
// loop assert via errors.Is for retry / budget decisions.
var (
	// ErrLLMHTTPStatus fires on any non-2xx response. The original
	// status code + body excerpt are wrapped in the returned error's
	// message so logs carry enough for triage.
	ErrLLMHTTPStatus = errors.New("llm: non-2xx HTTP status")

	// ErrLLMRateLimit fires specifically on HTTP 429. Separate from
	// the generic status error so the agent loop can implement
	// exponential backoff (Phase 5.4) without string-parsing.
	ErrLLMRateLimit = errors.New("llm: rate limited (HTTP 429)")

	// ErrLLMUnauthorized fires on 401 / 403. Callers surface this to
	// the operator as a config error (bad API key, wrong endpoint) —
	// retrying won't help.
	ErrLLMUnauthorized = errors.New("llm: auth failed (HTTP 401/403)")

	// ErrLLMMalformed fires when the provider returned JSON that
	// doesn't conform to the OpenAI chat-completions schema we
	// expect. Means an API drift or a broken relay — not retriable.
	ErrLLMMalformed = errors.New("llm: malformed provider response")
)

// LLMClient is the real OpenAI-compatible HTTP client. Stateless
// per call; safe to share across goroutines. Implements LLMProvider.
type LLMClient struct {
	endpoint   string // full URL to /chat/completions
	apiKey     string // sent as "Authorization: Bearer ..."
	model      string // default model when ChatRequest.Model is empty
	httpClient *http.Client
}

// LLMClientConfig tunes client construction. Zero-value is usable
// for the common case (provider defaults, 120s timeout).
type LLMClientConfig struct {
	// Endpoint is the /chat/completions URL. Required.
	Endpoint string

	// APIKey is the Bearer token for the Authorization header.
	// Resolved by caller (config.ResolveSecret) before constructing
	// the client — this field holds the plaintext. Empty is valid
	// for local providers (Ollama) that don't authenticate.
	APIKey string

	// Model is the default model name when ChatRequest.Model is empty.
	// Typically comes from ProviderConfig.Model.
	Model string

	// Timeout overrides DefaultLLMTimeout. Zero → default.
	Timeout time.Duration

	// HTTPClient lets callers inject a pre-configured client (for
	// proxies, keep-alive tuning, test doubles). Zero → a new client
	// with Timeout applied.
	HTTPClient *http.Client
}

// NewLLMClient constructs a client from explicit config. Fails
// fast if Endpoint is missing — empty endpoint with no fallback is
// a configuration bug, not a runtime recoverable.
func NewLLMClient(cfg LLMClientConfig) (*LLMClient, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("LLMClient: endpoint is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = DefaultLLMTimeout
		}
		hc = &http.Client{Timeout: timeout}
	}
	return &LLMClient{
		endpoint:   cfg.Endpoint,
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		httpClient: hc,
	}, nil
}

// NewLLMClientFromProvider constructs a client from a ResolveStep's
// resolved ProviderConfig. The caller is expected to have already
// resolved APIKeyRef into an actual key (via config.ResolveSecret
// at node startup).
func NewLLMClientFromProvider(p config.ProviderConfig, apiKey string) (*LLMClient, error) {
	return NewLLMClient(LLMClientConfig{
		Endpoint: p.Endpoint,
		APIKey:   apiKey,
		Model:    p.Model,
	})
}

// Chat satisfies LLMProvider. Marshals the request as OpenAI-shape
// JSON, POSTs to the endpoint, unmarshals the response, surfaces
// errors via the ErrLLM* sentinels where callers may want to branch.
func (c *LLMClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	body, err := json.Marshal(toOpenAIRequest(req, c.model))
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: build http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm: http do: %w", err)
	}
	defer resp.Body.Close()

	rawBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("llm: read response body: %w", readErr)
	}

	if resp.StatusCode >= 400 {
		return nil, classifyHTTPError(resp.StatusCode, rawBody)
	}

	var openResp openAIResponse
	if err := json.Unmarshal(rawBody, &openResp); err != nil {
		return nil, fmt.Errorf("%w: %v (body: %s)", ErrLLMMalformed, err, truncateBody(rawBody))
	}
	if len(openResp.Choices) == 0 {
		return nil, fmt.Errorf("%w: response had no choices", ErrLLMMalformed)
	}

	return fromOpenAIResponse(&openResp), nil
}

// classifyHTTPError turns a non-2xx response into the right sentinel
// wrapped with enough context (status + body excerpt) for triage.
func classifyHTTPError(status int, body []byte) error {
	excerpt := truncateBody(body)
	switch status {
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s", ErrLLMRateLimit, excerpt)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w (HTTP %d): %s", ErrLLMUnauthorized, status, excerpt)
	default:
		return fmt.Errorf("%w (HTTP %d): %s", ErrLLMHTTPStatus, status, excerpt)
	}
}

// truncateBody caps a body excerpt at 512 bytes so error messages
// don't carry a full malformed page payload into logs / telemetry.
func truncateBody(body []byte) string {
	const max = 512
	if len(body) <= max {
		return string(body)
	}
	return string(body[:max]) + "…[truncated]"
}

// ---------------------------------------------------------------
// OpenAI chat-completions wire shapes. Defined here (not in
// llm.go) to keep the Provider-agnostic interface distinct from
// this specific HTTP protocol. A future Anthropic-native client
// would share llm.go's types but define its own wire shapes.
// ---------------------------------------------------------------

type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float32         `json:"temperature,omitempty"`
	Tools       []openAITool    `json:"tools,omitempty"`
	ToolChoice  any             `json:"tool_choice,omitempty"` // string ("auto"/"none"/"required") or object
}

type openAIMessage struct {
	Role       string                `json:"role"`
	Content    string                `json:"content,omitempty"`
	ToolCalls  []openAIToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string                `json:"tool_call_id,omitempty"`
}

type openAITool struct {
	Type     string           `json:"type"` // always "function" for now
	Function openAIToolFunc   `json:"function"`
}

type openAIToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"` // "function"
	Function openAIToolCallFunc `json:"function"`
}

type openAIToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIResponse struct {
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

type openAIChoice struct {
	Message      openAIResponseMessage `json:"message"`
	FinishReason string                `json:"finish_reason"`
}

type openAIResponseMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []openAIToolCall `json:"tool_calls"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// Anthropic proxies often surface a `cached_tokens` field; keep
	// it here so cost accounting can use it when present.
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// toOpenAIRequest translates the provider-agnostic ChatRequest to
// the wire shape. Model defaults to the client's configured model
// when req.Model is empty.
func toOpenAIRequest(req ChatRequest, defaultModel string) openAIRequest {
	model := req.Model
	if model == "" {
		model = defaultModel
	}
	out := openAIRequest{
		Model:       model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	out.Messages = make([]openAIMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msg := openAIMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, openAIToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: openAIToolCallFunc{
					Name:      tc.Name,
					Arguments: tc.Arguments,
				},
			})
		}
		out.Messages = append(out.Messages, msg)
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, openAITool{
			Type: "function",
			Function: openAIToolFunc{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	if req.ToolChoice != "" {
		out.ToolChoice = req.ToolChoice
	}
	return out
}

// fromOpenAIResponse translates the wire shape back to the
// provider-agnostic ChatResponse. Takes the first choice (we don't
// request n > 1); subsequent choices are ignored.
func fromOpenAIResponse(r *openAIResponse) *ChatResponse {
	first := r.Choices[0]
	out := &ChatResponse{
		Content:      first.Message.Content,
		FinishReason: first.FinishReason,
		Usage: Usage{
			PromptTokens:     r.Usage.PromptTokens,
			CompletionTokens: r.Usage.CompletionTokens,
			TotalTokens:      r.Usage.TotalTokens,
			CachedTokens:     r.Usage.CachedTokens,
		},
	}
	for _, tc := range first.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return out
}
