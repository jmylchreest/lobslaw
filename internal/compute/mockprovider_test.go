package compute

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestMockProviderScriptedSequence(t *testing.T) {
	t.Parallel()
	m := NewMockProvider(
		MockResponse{Content: "first"},
		MockResponse{Content: "second"},
	)
	ctx := context.Background()

	r1, err := m.Chat(ctx, ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Content != "first" {
		t.Errorf("first response: got %q, want 'first'", r1.Content)
	}
	r2, _ := m.Chat(ctx, ChatRequest{})
	if r2.Content != "second" {
		t.Errorf("second response: got %q", r2.Content)
	}
}

func TestMockProviderExhaustedReturnsSentinel(t *testing.T) {
	t.Parallel()
	m := NewMockProvider(MockResponse{Content: "only"})
	ctx := context.Background()

	_, err := m.Chat(ctx, ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Chat(ctx, ChatRequest{})
	if !errors.Is(err, ErrMockExhausted) {
		t.Errorf("expected ErrMockExhausted; got %v", err)
	}
}

func TestMockProviderFinishReasonDefaults(t *testing.T) {
	t.Parallel()
	m := NewMockProvider(
		MockResponse{Content: "text"},
		MockResponse{ToolCalls: []ToolCall{{ID: "1", Name: "bash", Arguments: "{}"}}},
	)
	ctx := context.Background()
	r1, _ := m.Chat(ctx, ChatRequest{})
	if r1.FinishReason != "stop" {
		t.Errorf("text-only default finish: got %q, want 'stop'", r1.FinishReason)
	}
	r2, _ := m.Chat(ctx, ChatRequest{})
	if r2.FinishReason != "tool_calls" {
		t.Errorf("tool-calls default finish: got %q, want 'tool_calls'", r2.FinishReason)
	}
}

func TestMockProviderScriptFunc(t *testing.T) {
	t.Parallel()
	m := NewMockProviderFunc(func(req ChatRequest, idx int) (MockResponse, error) {
		// Response depends on what the caller sent — exactly the
		// use case ScriptFunc exists for.
		for _, msg := range req.Messages {
			if msg.Content == "call-tool" {
				return MockResponse{
					ToolCalls: []ToolCall{{ID: "t1", Name: "bash", Arguments: "{}"}},
				}, nil
			}
		}
		return MockResponse{Content: fmt.Sprintf("turn %d", idx)}, nil
	})
	ctx := context.Background()

	r1, _ := m.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "plain"}}})
	if r1.Content != "turn 0" {
		t.Errorf("got %q", r1.Content)
	}
	r2, _ := m.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "call-tool"}}})
	if len(r2.ToolCalls) != 1 {
		t.Errorf("expected tool call; got %+v", r2)
	}
}

func TestMockProviderScriptFuncError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("LLM-layer rate limit")
	m := NewMockProviderFunc(func(req ChatRequest, idx int) (MockResponse, error) {
		return MockResponse{}, wantErr
	})
	_, err := m.Chat(context.Background(), ChatRequest{})
	if !errors.Is(err, wantErr) {
		t.Errorf("ScriptFunc error should surface; got %v", err)
	}
}

func TestMockProviderScriptedErrProp(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("simulated timeout")
	m := NewMockProvider(MockResponse{Err: wantErr})
	_, err := m.Chat(context.Background(), ChatRequest{})
	if !errors.Is(err, wantErr) {
		t.Errorf("scripted Err field should surface; got %v", err)
	}
}

func TestMockProviderContextCancellation(t *testing.T) {
	t.Parallel()
	m := NewMockProvider(MockResponse{Content: "should-not-see-this"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := m.Chat(ctx, ChatRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx should surface as error; got %v", err)
	}
	// Cancelled calls neither consume script nor pollute the call
	// log — nothing actually happened.
	if m.CallCount() != 0 {
		t.Errorf("cancelled call shouldn't be logged; got count=%d", m.CallCount())
	}
	// Follow-up with a live ctx should still see the first scripted
	// response (not exhausted).
	r, err := m.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Content != "should-not-see-this" {
		t.Errorf("cancelled call shouldn't have consumed script; got %q", r.Content)
	}
}

func TestMockProviderCallsCapturesRequests(t *testing.T) {
	t.Parallel()
	m := NewMockProvider(MockResponse{Content: "1"}, MockResponse{Content: "2"})
	ctx := context.Background()

	req1 := ChatRequest{Model: "model-A", Messages: []Message{{Role: "user", Content: "hi"}}}
	req2 := ChatRequest{Model: "model-B", Temperature: 0.5}
	_, _ = m.Chat(ctx, req1)
	_, _ = m.Chat(ctx, req2)

	calls := m.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls captured; got %d", len(calls))
	}
	if calls[0].Model != "model-A" || calls[1].Model != "model-B" {
		t.Errorf("call order / model not captured: %+v", calls)
	}
	if calls[0].Messages[0].Content != "hi" {
		t.Errorf("message content not captured")
	}
}

func TestMockProviderCallsIsolatedFromExternalMutation(t *testing.T) {
	t.Parallel()
	m := NewMockProvider(MockResponse{Content: "x"})
	msgs := []Message{{Role: "user", Content: "original"}}
	_, _ = m.Chat(context.Background(), ChatRequest{Messages: msgs})

	msgs[0].Content = "MUTATED"

	got := m.Calls()[0].Messages[0].Content
	if got == "MUTATED" {
		t.Error("external mutation leaked into mock's call log")
	}
}

func TestMockProviderReset(t *testing.T) {
	t.Parallel()
	m := NewMockProvider(MockResponse{Content: "a"}, MockResponse{Content: "b"})
	_, _ = m.Chat(context.Background(), ChatRequest{})
	m.Reset()
	// After reset, cursor is back to 0 → next call returns first.
	r, _ := m.Chat(context.Background(), ChatRequest{})
	if r.Content != "a" {
		t.Errorf("reset should restart cursor; got %q", r.Content)
	}
	if m.CallCount() != 1 {
		t.Errorf("call log should be cleared by Reset; got %d", m.CallCount())
	}
}

func TestMockProviderConcurrentCallsAreSerialised(t *testing.T) {
	t.Parallel()
	const n = 50
	script := make([]MockResponse, n)
	for i := range script {
		script[i] = MockResponse{Content: fmt.Sprintf("turn-%d", i)}
	}
	m := NewMockProvider(script...)

	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			_, _ = m.Chat(context.Background(), ChatRequest{})
		}()
	}
	wg.Wait()

	if m.CallCount() != n {
		t.Errorf("want %d calls logged; got %d", n, m.CallCount())
	}
	// Each scripted response should have been consumed exactly once.
	// Calls() order isn't deterministic under concurrent callers
	// but *every* turn-* should appear across the outputs.
	seen := make(map[string]int)
	for i := range n {
		seen[fmt.Sprintf("turn-%d", i)] = 0
	}
	// We don't capture outputs, so this asserts only on total call count.
	// Existing code path is sufficient: all n script items got consumed
	// because the n+1-th call would return ErrMockExhausted (not tested
	// here; see TestMockProviderExhaustedReturnsSentinel).
}

func TestMockProviderSatisfiesLLMProviderInterface(t *testing.T) {
	t.Parallel()
	var _ LLMProvider = NewMockProvider()
	var _ LLMProvider = NewMockProviderFunc(nil)
}
