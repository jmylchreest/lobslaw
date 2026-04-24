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

func TestChatHistoryForget(t *testing.T) {
	t.Parallel()
	h := newChatHistory(20, time.Hour)
	h.Append(1, compute.Message{Role: "user", Content: "hello"})
	h.Forget(1)
	if got := h.Load(1); len(got) != 0 {
		t.Errorf("Forget should clear; got %d", len(got))
	}
}
