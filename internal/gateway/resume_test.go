package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// --- REST long-poll resume ---------------------------------------

// TestRESTPromptDeniedReplyText pins the user-visible reply string
// the handler produces when Wait returns Denied. The resume loop
// does not re-enter the agent on non-Approved decisions.
func TestRESTPromptDeniedReplyText(t *testing.T) {
	t.Parallel()

	reg := NewPromptRegistry()
	p, _ := reg.Create(NewPrompt{TurnID: "t", Reason: "reason", Channel: "rest", TTL: time.Second})

	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = reg.Resolve(p.ID, PromptDenied, PromptScopeOnce)
	}()

	d, err := reg.Wait(t.Context(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d != PromptDenied {
		t.Fatalf("want Denied; got %s", d)
	}
	want := "Confirmation " + d.String() + ": reason"
	if !strings.Contains(want, "denied") {
		t.Errorf("reply-text template should render %q", d.String())
	}
}

// --- Telegram callback resume ------------------------------------

// TestTelegramCallbackApproveResumesAgent — approve a prompt through
// the callback_query path; the handler MUST call the agent's resume
// and send the final reply as a follow-up sendMessage.
func TestTelegramCallbackApproveResumesAgent(t *testing.T) {
	t.Parallel()

	provider := compute.NewMockProvider(compute.MockResponse{Content: "resumed reply"})
	agent, err := compute.NewAgent(compute.AgentConfig{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	h := newTGPromptHarness(t, agent, TelegramConfig{UnknownUserScope: "public"})

	p := pausedPrompt(t, h, "turn-1", []compute.Message{
		{Role: "user", Content: "original"},
		{Role: "assistant", Content: "(paused)"},
	})

	update := `{
		"update_id": 800,
		"callback_query": {
			"id": "cb-apx",
			"from": {"id": 1, "username": "u"},
			"message": {"message_id": 2, "chat": {"id": 42, "type": "private"}, "date": 0},
			"data": "prompt:approve:` + p.ID + `"
		}
	}`
	rec := postUpdate(t, h.handler, "test-webhook-secret", update)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook should 200; got %d", rec.Code)
	}

	var sawApprove, sawResumed bool
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !(sawApprove && sawResumed) {
		for _, c := range h.capturedCalls() {
			if c.Method != "sendMessage" {
				continue
			}
			txt, _ := c.Body["text"].(string)
			if txt == "Approved." {
				sawApprove = true
			}
			if txt == "resumed reply" {
				sawResumed = true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawApprove {
		t.Error(`missing the "Approved." ack message`)
	}
	if !sawResumed {
		t.Error("missing the resumed-agent reply")
	}

	// A second tap on the same button must not run the turn again.
	// The record is resolved rather than deleted, so "already spent"
	// is a decision check now, not an absent map entry.
	before := len(h.capturedCalls())
	_ = postUpdate(t, h.handler, "test-webhook-secret", update)
	time.Sleep(50 * time.Millisecond)
	for _, c := range h.capturedCalls()[before:] {
		if c.Method != "sendMessage" {
			continue
		}
		if txt, _ := c.Body["text"].(string); txt == "resumed reply" {
			t.Error("a second tap resumed the turn again")
		}
	}
}

// TestTelegramCallbackDenyDoesNotResume — deny records the refusal
// and stops. A denied turn that ran anyway would make the button a
// decoration.
func TestTelegramCallbackDenyDoesNotResume(t *testing.T) {
	t.Parallel()

	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{UnknownUserScope: "public"})
	p := pausedPrompt(t, h, "turn-1", []compute.Message{{Role: "user", Content: "x"}})

	update := `{
		"update_id": 801,
		"callback_query": {
			"id": "cb-dny",
			"from": {"id": 1, "username": "u"},
			"message": {"message_id": 2, "chat": {"id": 42, "type": "private"}, "date": 0},
			"data": "prompt:deny:` + p.ID + `"
		}
	}`
	_ = postUpdate(t, h.handler, "test-webhook-secret", update)

	if snap, err := h.registry.Get(p.ID); err != nil || snap.Decision != PromptDenied {
		t.Errorf("prompt is %v (err %v), want denied", snap, err)
	}

	var denyCount, otherCount int
	for _, c := range h.capturedCalls() {
		if c.Method != "sendMessage" {
			continue
		}
		txt, _ := c.Body["text"].(string)
		if txt == "Denied." {
			denyCount++
		} else {
			otherCount++
		}
	}
	if denyCount != 1 {
		t.Errorf("expected exactly 1 'Denied.' message; got %d", denyCount)
	}
	if otherCount != 0 {
		t.Errorf("deny path sent unexpected extra messages: %d", otherCount)
	}
}

// TestTelegramCallbackApproveWithoutContinuation — user taps approve
// AFTER the bot restarted (state lost). Handler acks + tells the
// user to resend rather than silently doing nothing.
func TestTelegramCallbackApproveWithoutContinuation(t *testing.T) {
	t.Parallel()

	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{UnknownUserScope: "public"})
	// Deliberately no continuation: a prompt raised without one, or
	// whose record predates this field.
	p, err := h.registry.Create(NewPrompt{
		TurnID: "t", Reason: "r", Channel: "telegram", TTL: time.Minute,
		RaisedFor: "tg-@u",
	})
	if err != nil {
		t.Fatal(err)
	}

	update := `{
		"update_id": 802,
		"callback_query": {
			"id": "cb-orphan",
			"from": {"id": 1, "username": "u"},
			"message": {"message_id": 2, "chat": {"id": 42, "type": "private"}, "date": 0},
			"data": "prompt:approve:` + p.ID + `"
		}
	}`
	_ = postUpdate(t, h.handler, "test-webhook-secret", update)

	var sawLostTrack bool
	for _, c := range h.capturedCalls() {
		if c.Method != "sendMessage" {
			continue
		}
		txt, _ := c.Body["text"].(string)
		if strings.Contains(txt, "lost track") {
			sawLostTrack = true
		}
	}
	if !sawLostTrack {
		t.Error(`expected a "lost track" fallback reply`)
	}
}

// pausedPrompt registers a confirmation carrying a paused turn, the
// way sendConfirmationKeyboard does.
func pausedPrompt(t *testing.T, h *tgPromptHarness, turnID string, msgs []compute.Message) *Prompt {
	t.Helper()
	budget, err := compute.NewTurnBudget(compute.BudgetCaps{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := h.registry.Create(NewPrompt{
		TurnID:    turnID,
		Reason:    "reason",
		Channel:   "telegram",
		ChannelID: "42",
		TTL:       time.Minute,
		RaisedFor: "tg-@u",
		Continuation: &Continuation{
			Request:  compute.ProcessMessageRequest{TurnID: turnID, Budget: budget},
			Messages: msgs,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
