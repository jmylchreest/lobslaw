package compute

import (
	"context"
	"strings"
	"testing"
)

func TestSummarizerSendsPriorSummaryAndMessages(t *testing.T) {
	t.Parallel()
	provider := NewMockProvider(MockResponse{Content: "the user is called james"})
	s := NewLLMSummarizer(provider, "test-model", SummarizerConfig{})

	got, err := s.SummarizeConversation(context.Background(), "earlier: they said hello",
		[]Message{{Role: "user", Content: "my name is james"}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "the user is called james" {
		t.Errorf("summary = %q", got)
	}
	calls := provider.Calls()
	if len(calls) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(calls))
	}
	// The harness's own framing and the transcript travel in
	// different messages. The prior summary is this system's output,
	// the transcript is not, and one string containing both left a
	// line of prose as the only boundary — which an injected message
	// can imitate.
	system, transcript := calls[0].Messages[0].Content, calls[0].Messages[1].Content
	if !strings.Contains(system, "earlier: they said hello") {
		t.Error("prior summary not carried into the instructions")
	}
	if strings.Contains(transcript, "earlier: they said hello") {
		t.Error("the prior summary is in the transcript message; it is not part of the transcript")
	}
	if !strings.Contains(transcript, "my name is james") {
		t.Error("new messages not carried into the prompt")
	}
}

func TestSummarizerNoPriorReadsAsConversationStart(t *testing.T) {
	t.Parallel()
	provider := NewMockProvider(MockResponse{Content: "ok"})
	s := NewLLMSummarizer(provider, "m", SummarizerConfig{})
	if _, err := s.SummarizeConversation(context.Background(), "", []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	system := provider.Calls()[0].Messages[0].Content
	if !strings.Contains(system, "no summary yet") {
		t.Error("first compaction should say so")
	}
}

// The summariser exists to save tokens; shipping it 10 MB of grep
// output would defeat that on the very call meant to help.
func TestSummarizerTruncatesToolResults(t *testing.T) {
	t.Parallel()
	provider := NewMockProvider(MockResponse{Content: "ok"})
	s := NewLLMSummarizer(provider, "m", SummarizerConfig{})
	huge := strings.Repeat("match\n", 5000)

	if _, err := s.SummarizeConversation(context.Background(), "", []Message{
		{Role: "tool", ToolCallID: "c1", Content: huge},
	}); err != nil {
		t.Fatal(err)
	}
	prompt := provider.Calls()[0].Messages[1].Content
	if len(prompt) > 2000 {
		t.Errorf("prompt is %d bytes; tool output should have been truncated", len(prompt))
	}
	if !strings.Contains(prompt, "bytes total") {
		t.Error("truncation should say how much was dropped")
	}
}

func TestSummarizerNamesToolCalls(t *testing.T) {
	t.Parallel()
	provider := NewMockProvider(MockResponse{Content: "ok"})
	s := NewLLMSummarizer(provider, "m", SummarizerConfig{})
	if _, err := s.SummarizeConversation(context.Background(), "", []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{Name: "shell_command"}, {Name: "read_file"}}},
	}); err != nil {
		t.Fatal(err)
	}
	prompt := provider.Calls()[0].Messages[1].Content
	if !strings.Contains(prompt, "shell_command") || !strings.Contains(prompt, "read_file") {
		t.Errorf("tool names should survive into the summary prompt: %q", prompt)
	}
}

func TestSummarizerEmptyBatchReturnsPriorUnchanged(t *testing.T) {
	t.Parallel()
	provider := NewMockProvider()
	s := NewLLMSummarizer(provider, "m", SummarizerConfig{})
	got, err := s.SummarizeConversation(context.Background(), "unchanged", nil)
	if err != nil || got != "unchanged" {
		t.Errorf("got %q, %v; want the prior summary and no provider call", got, err)
	}
	if provider.CallCount() != 0 {
		t.Error("empty batch should not cost an LLM call")
	}
}

func TestNewLLMSummarizerNilWithoutProvider(t *testing.T) {
	t.Parallel()
	if s := NewLLMSummarizer(nil, "m", SummarizerConfig{}); s != nil {
		t.Error("no provider should mean no summariser (compaction off)")
	}
}

func TestSummarizerHonoursConfiguredCaps(t *testing.T) {
	t.Parallel()
	provider := NewMockProvider(MockResponse{Content: "ok"})
	s := NewLLMSummarizer(provider, "m", SummarizerConfig{
		MaxCompletionTokens: 77,
		ToolResultBytes:     20,
	})
	huge := strings.Repeat("x", 5000)
	if _, err := s.SummarizeConversation(context.Background(), "", []Message{
		{Role: "tool", ToolCallID: "c1", Content: huge},
	}); err != nil {
		t.Fatal(err)
	}
	call := provider.Calls()[0]
	if call.MaxTokens != 77 {
		t.Errorf("MaxTokens = %d, want the configured 77", call.MaxTokens)
	}
	body := call.Messages[1].Content
	if strings.Count(body, "x") > 40 {
		t.Errorf("tool result not truncated to the configured 20 bytes: %d x's", strings.Count(body, "x"))
	}
}

// Operators can name what their deployment must never lose, without
// losing the built-in guidance that stops the model narrating.
func TestSummarizerAppendsExtraInstructions(t *testing.T) {
	t.Parallel()
	provider := NewMockProvider(MockResponse{Content: "ok"})
	s := NewLLMSummarizer(provider, "m", SummarizerConfig{
		ExtraInstructions: "always keep ticket numbers",
	})
	if _, err := s.SummarizeConversation(context.Background(), "", []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	system := provider.Calls()[0].Messages[0].Content
	if !strings.Contains(system, "always keep ticket numbers") {
		t.Error("extra instructions missing from the system prompt")
	}
	if !strings.Contains(system, "Never invent detail") {
		t.Error("extra instructions replaced the built-in prompt instead of extending it")
	}
}
