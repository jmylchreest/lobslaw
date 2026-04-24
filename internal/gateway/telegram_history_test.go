package gateway

import (
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

func TestChatHistoryAppendAndLoad(t *testing.T) {
	t.Parallel()
	h := newChatHistory(20, 5*time.Minute)
	h.Append(42,
		compute.Message{Role: "user", Content: "hi"},
		compute.Message{Role: "assistant", Content: "hey"})
	got := h.Load(42)
	if len(got) != 2 {
		t.Fatalf("Load = %d; want 2", len(got))
	}
	if got[0].Role != "user" || got[1].Role != "assistant" {
		t.Errorf("order lost: %+v", got)
	}
}

func TestChatHistoryCapsAtMaxTurns(t *testing.T) {
	t.Parallel()
	h := newChatHistory(4, time.Hour)
	for i := 0; i < 10; i++ {
		h.Append(1, compute.Message{Role: "user", Content: "msg"})
	}
	got := h.Load(1)
	if len(got) != 4 {
		t.Errorf("cap = %d; want 4", len(got))
	}
}

func TestChatHistoryEvictsStale(t *testing.T) {
	t.Parallel()
	h := newChatHistory(20, 10*time.Millisecond)
	h.Append(1, compute.Message{Role: "user", Content: "early"})
	time.Sleep(25 * time.Millisecond)
	if got := h.Load(1); len(got) != 0 {
		t.Errorf("expired bucket should Load empty; got %d msgs", len(got))
	}
}

func TestChatHistorySeparatesByChat(t *testing.T) {
	t.Parallel()
	h := newChatHistory(20, time.Hour)
	h.Append(1, compute.Message{Role: "user", Content: "alice"})
	h.Append(2, compute.Message{Role: "user", Content: "bob"})
	a := h.Load(1)
	b := h.Load(2)
	if len(a) != 1 || a[0].Content != "alice" {
		t.Errorf("chat 1: %+v", a)
	}
	if len(b) != 1 || b[0].Content != "bob" {
		t.Errorf("chat 2: %+v", b)
	}
}

func TestNewTurnMessagesStripsSystemAndPrior(t *testing.T) {
	t.Parallel()
	all := []compute.Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "old user msg"},
		{Role: "assistant", Content: "old assistant reply"},
		{Role: "user", Content: "new user msg"},
		{Role: "assistant", ToolCalls: []compute.ToolCall{{Name: "fetch_url"}}},
		{Role: "tool", Content: "tool result"},
		{Role: "assistant", Content: "final reply"},
	}
	priorLen := 2 // old user msg + old assistant reply
	got := newTurnMessages(all, priorLen)
	if len(got) != 4 {
		t.Fatalf("len=%d want 4 (user + asst-tool-call + tool result + final asst)", len(got))
	}
	if got[0].Role != "user" || got[0].Content != "new user msg" {
		t.Errorf("first new msg: %+v", got[0])
	}
	if len(got[1].ToolCalls) != 1 {
		t.Errorf("tool call should survive: %+v", got[1])
	}
	if got[2].Role != "tool" {
		t.Errorf("tool result should survive: %+v", got[2])
	}
	if got[3].Content != "final reply" {
		t.Errorf("final reply: %+v", got[3])
	}
}

func TestNewTurnMessagesNoSystemPrefix(t *testing.T) {
	t.Parallel()
	all := []compute.Message{
		{Role: "user", Content: "u"},
		{Role: "assistant", Content: "a"},
	}
	got := newTurnMessages(all, 0)
	if len(got) != 2 {
		t.Errorf("without system prefix all messages should appear: %+v", got)
	}
}

func TestNewTurnMessagesPriorOverflowSafe(t *testing.T) {
	t.Parallel()
	all := []compute.Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
	}
	// Defensive: priorLen larger than what's actually in the slice
	// shouldn't crash; should return empty.
	if got := newTurnMessages(all, 100); got != nil {
		t.Errorf("overflow should yield nil; got %+v", got)
	}
}

func TestChatHistoryForget(t *testing.T) {
	t.Parallel()
	h := newChatHistory(20, time.Hour)
	h.Append(1, compute.Message{Role: "user", Content: "hello"})
	h.Forget(1)
	if got := h.Load(1); len(got) != 0 {
		t.Errorf("Forget should clear; got %d", len(got))
	}
}
