package compute

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func quietAgent() *Agent {
	return &Agent{cfg: AgentConfig{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
}

// A turn whose last message is system-role leaves the model answering
// the system prompt — which is how a Telegram reply came back agreeing
// to its own instructions and describing its own configuration.
func TestSeedMessagesNeverEndsOnTheSystemPrompt(t *testing.T) {
	t.Parallel()
	msgs := quietAgent().seedMessages(ProcessMessageRequest{
		SystemPrompt: "# Identity\n\nyou are a bot",
		Message:      "",
	})
	if len(msgs) == 0 {
		t.Fatal("expected at least the system prompt")
	}
	last := msgs[len(msgs)-1]
	if last.Role == "system" {
		t.Fatal("the turn ends on system role; the model would be replying to its own prompt")
	}
	if !strings.Contains(last.Content, "no message content") {
		t.Errorf("expected the placeholder turn, got %q", last.Content)
	}
}

// The backstop must not fire on a well-formed turn, or it appends a
// second user message after the real one.
func TestSeedMessagesLeavesARealTurnAlone(t *testing.T) {
	t.Parallel()
	msgs := quietAgent().seedMessages(ProcessMessageRequest{
		SystemPrompt: "# Identity",
		Message:      "what's the time?",
	})
	last := msgs[len(msgs)-1]
	if last.Content != "what's the time?" {
		t.Fatalf("the user's message should be last, got %q", last.Content)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "no message content") {
			t.Fatal("the backstop fired on a well-formed turn")
		}
	}
}

// A transcript that ends on a tool result is a turn the model can
// continue — the check is "not system", not "is user".
func TestSeedMessagesAcceptsATrailingToolResult(t *testing.T) {
	t.Parallel()
	msgs := quietAgent().seedMessages(ProcessMessageRequest{
		SystemPrompt: "# Identity",
		ConversationHistory: []Message{
			{Role: "user", Content: "check the weather"},
			{Role: "assistant", Content: ""},
			{Role: "tool", Content: `{"temp":11}`},
		},
	})
	for _, m := range msgs {
		if strings.Contains(m.Content, "no message content") {
			t.Fatal("the backstop fired on a resumable tool turn")
		}
	}
}
