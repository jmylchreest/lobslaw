package compute

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// MockProvider is a scripted LLMProvider for tests. Two operating
// modes:
//
//  1. Scripted sequence — supply a fixed list of responses; Chat()
//     returns them in order. Once exhausted, Chat returns
//     ErrMockExhausted (tests should assert this or extend the script).
//  2. Function — supply a ScriptFunc; Chat() calls it with the
//     incoming request and returns whatever it produces. Useful when
//     the test needs to react to prompt content (e.g. "if the prompt
//     mentions cheese, call a specific tool").
//
// Safe for concurrent use; calls are serialised internally so the
// scripted sequence is deterministic even under parallel Chat()
// invocations.
type MockProvider struct {
	mu        sync.Mutex
	script    []MockResponse
	cursor    int
	scriptFn  ScriptFunc
	callLog   []ChatRequest
}

// ScriptFunc is the dynamic counterpart to a fixed script. Receives
// the incoming request + the call index (0-based) and returns the
// response. Useful for tests that need to assert on prompt content.
type ScriptFunc func(req ChatRequest, callIndex int) (MockResponse, error)

// MockResponse is one scripted turn. A response without ToolCalls
// is a text reply; a response with ToolCalls signals the agent
// loop to invoke tools and come back for another round.
type MockResponse struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
	Err          error // returned directly from Chat if non-nil
}

// ErrMockExhausted fires when a scripted sequence is consumed and
// another Chat() is attempted. Tests should either assert this or
// pad the script with a final "model stopped" response.
var ErrMockExhausted = errors.New("mock provider: scripted sequence exhausted")

// NewMockProvider constructs a scripted-sequence mock.
func NewMockProvider(script ...MockResponse) *MockProvider {
	return &MockProvider{script: append([]MockResponse(nil), script...)}
}

// NewMockProviderFunc constructs a dynamic-response mock.
func NewMockProviderFunc(fn ScriptFunc) *MockProvider {
	return &MockProvider{scriptFn: fn}
}

// Chat satisfies LLMProvider. Dispatches to the fixed script or the
// dynamic ScriptFunc depending on how the mock was constructed.
// Logs every incoming request so tests can assert on assembled
// prompts after the fact via Calls().
func (m *MockProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Respect context cancellation — matches real client behaviour,
	// avoids tests that check for ctx.Err() being noisily incorrect.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	idx := len(m.callLog)
	m.callLog = append(m.callLog, cloneRequest(req))

	if m.scriptFn != nil {
		resp, err := m.scriptFn(req, idx)
		if err != nil {
			return nil, err
		}
		return mockResponseToChat(resp), resp.Err
	}

	if m.cursor >= len(m.script) {
		return nil, fmt.Errorf("%w (call %d)", ErrMockExhausted, idx)
	}
	resp := m.script[m.cursor]
	m.cursor++
	if resp.Err != nil {
		return nil, resp.Err
	}
	return mockResponseToChat(resp), nil
}

// Calls returns a snapshot of every request Chat() has received.
// Tests use this to assert on assembled system prompts, tool
// definitions, temperature, etc.
func (m *MockProvider) Calls() []ChatRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ChatRequest, len(m.callLog))
	copy(out, m.callLog)
	return out
}

// CallCount reports how many times Chat() has been invoked.
// Equivalent to len(Calls()) without the allocation.
func (m *MockProvider) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.callLog)
}

// Reset clears the call log and resets the scripted cursor to the
// start. Useful between test cases that share a mock.
func (m *MockProvider) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cursor = 0
	m.callLog = nil
}

// mockResponseToChat lifts a MockResponse into the ChatResponse
// shape. Populates FinishReason defaults when the script left it
// empty — "tool_calls" when tools were requested, "stop" otherwise.
func mockResponseToChat(r MockResponse) *ChatResponse {
	reason := r.FinishReason
	if reason == "" {
		if len(r.ToolCalls) > 0 {
			reason = "tool_calls"
		} else {
			reason = "stop"
		}
	}
	return &ChatResponse{
		Content:      r.Content,
		ToolCalls:    append([]ToolCall(nil), r.ToolCalls...),
		FinishReason: reason,
		Usage:        r.Usage,
	}
}

// cloneRequest copies ChatRequest slices so the test can mutate
// its own slices after a Chat() call without the mock's call log
// changing underneath assertions.
func cloneRequest(req ChatRequest) ChatRequest {
	out := req
	if req.Messages != nil {
		out.Messages = append([]Message(nil), req.Messages...)
	}
	if req.Tools != nil {
		out.Tools = append([]Tool(nil), req.Tools...)
	}
	return out
}
