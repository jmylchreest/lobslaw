package compute

import (
	"context"
	"encoding/json"
)

// LLMProvider is the minimal contract the agent loop needs from an
// LLM. Kept deliberately narrow — advanced features (fine-tuning,
// embeddings) belong on their own interfaces; Chat covers the
// conversational tool-calling loop that RunToolCallLoop drives.
//
// Implementations must be safe to call from multiple goroutines.
// The real OpenAI-compatible client (internal/compute/llmclient.go)
// and the MockProvider (internal/compute/mockprovider.go) both
// satisfy this.
type LLMProvider interface {
	// Chat executes one round-trip. Returns the assistant's message
	// plus any tool-calls the model wants to make. Non-streaming in
	// this interface; streaming lands later if a channel needs it.
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

// ChatRequest is the inputs to one LLM call. Matches the shape of
// OpenAI's /chat/completions endpoint closely so the real client
// is a thin translator; the mock provider accepts the same shape
// and returns scripted responses.
type ChatRequest struct {
	// Messages is the conversation in order. First message is
	// typically the system prompt from promptgen; subsequent
	// messages alternate user / assistant / tool per the usual
	// chat pattern.
	Messages []Message

	// Model names the target model. The resolver has already
	// picked the provider; Model is that provider's specific
	// deployment tag ("gpt-4o-mini", "claude-sonnet-4-6", etc.).
	Model string

	// MaxTokens caps the completion length. 0 → provider default.
	MaxTokens int

	// Temperature in [0, 2] — higher = more random. 0 → provider
	// default (usually 1.0).
	Temperature float32

	// Tools are the tool-call definitions the model may invoke.
	// Empty slice → text-only completion (no tool calling).
	Tools []Tool

	// ServerTools are provider-executed entries (OpenRouter's
	// openrouter:web_search etc.) merged into the wire tools array.
	// Not dispatched through our Executor — the provider handles
	// them and returns synthesised content.
	ServerTools []ServerTool

	// ToolChoice controls tool-use eagerness. Values:
	// "auto" — model decides (default when Tools is non-empty)
	// "none" — model must NOT call tools
	// "required" — model MUST call at least one tool
	// any other string — force a specific tool by name
	ToolChoice string
}

// Message is one turn in the conversation. Role + content match
// OpenAI's shape; ToolCalls / ToolCallID are populated for the
// tool-calling round-trip.
type Message struct {
	// Role is one of "system" | "user" | "assistant" | "tool".
	Role string

	// Content is the text of the message. For role="tool" this is
	// the tool's output, typically wrapped in untrusted delimiters
	// before being placed here.
	Content string

	// ToolCalls is populated on assistant messages that requested
	// one or more tool invocations.
	ToolCalls []ToolCall

	// ToolCallID is populated on tool-result messages (role="tool")
	// and correlates to the originating assistant ToolCall.ID.
	ToolCallID string
}

// Tool describes a callable tool for the LLM's tool-calling
// machinery. Parameters is a JSON schema blob — the model uses it
// to structure its arguments.
type Tool struct {
	Name        string
	Description string
	// Parameters is a JSON schema object (raw JSON). Callers build
	// it from the lobslaw Tool registry; shape matches OpenAI's
	// function-tool schema.
	Parameters json.RawMessage
}

// ServerTool is a provider-side tool — the provider runs it and
// synthesises a reply the model continues from; our Executor never
// sees it. Example: OpenRouter's openrouter:web_search. Type is
// the provider-specific discriminator; Parameters is an opaque
// object the provider interprets.
type ServerTool struct {
	Type       string
	Parameters map[string]any
}

// ToolCall is the model's request to invoke a tool. ID is the
// round-trip correlation identifier (assistant says "call X with
// args"; the subsequent tool-role message carries ID matching).
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON-encoded arguments
}

// ChatResponse is the outputs of one LLM call.
type ChatResponse struct {
	// Content is the assistant's text reply. May be empty if the
	// model chose to call tools instead.
	Content string

	// ToolCalls is populated when the model requested tool
	// invocations. Either Content or ToolCalls will be non-empty
	// (a well-behaved provider returns at least one).
	ToolCalls []ToolCall

	// FinishReason is the provider's termination reason:
	// "stop" (natural stop), "length" (hit max_tokens),
	// "tool_calls" (model called tools, handoff to caller),
	// "content_filter" (provider refused).
	FinishReason string

	// Usage is token counting for accounting / budget enforcement.
	Usage Usage
}

// Usage tracks token counts per call. CachedTokens is populated
// when the provider supports prompt caching (Anthropic in
// particular); zero on providers that don't.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     int
}
