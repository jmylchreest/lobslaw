package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// A prompt id is 128 bits of randomness, which makes it unguessable —
// but unguessable is not the same as authorised. The button is
// rendered into a chat, and in a GROUP every member can see it and tap
// it. Before this guard, the person who answered a confirmation was
// simply whoever got there first.

// tapPrompt fires a callback_query from a given sender.
func tapPrompt(t *testing.T, h *tgPromptHarness, promptID, from string) *http.Response {
	t.Helper()
	update := `{
		"update_id": 900,
		"callback_query": {
			"id": "cb-auth",
			"from": ` + from + `,
			"message": {"message_id": 2, "chat": {"id": 99, "type": "group"}, "date": 0},
			"data": "prompt:approve:` + promptID + `"
		}
	}`
	rec := postUpdate(t, h.handler, "test-webhook-secret", update)
	if rec.Code != http.StatusOK {
		// Always 200: refusing the tap is not refusing the webhook, and
		// a non-200 makes Telegram redeliver forever.
		t.Fatalf("callback webhook should 200; got %d", rec.Code)
	}
	return rec.Result()
}

// THE PROPERTY. Somebody else's confirmation is not yours to answer.
func TestACallbackFromAnotherUserIsRefused(t *testing.T) {
	t.Parallel()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{UnknownUserScope: "public"})

	p, _ := h.registry.Create(NewPrompt{
		TurnID: "turn-x", Reason: "reason", Channel: "telegram",
		TTL: time.Minute, RaisedFor: "tg-@alice",
	})

	tapPrompt(t, h, p.ID, `{"id": 2, "username": "mallory"}`)

	snap, _ := h.registry.Get(p.ID)
	if snap.Decision != PromptPending {
		t.Fatalf("a bystander resolved the prompt: %s", snap.Decision)
	}
}

// And the person it WAS asked of still gets through, or the guard has
// simply broken confirmations.
func TestTheUserItWasAskedOfCanAnswer(t *testing.T) {
	t.Parallel()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{UnknownUserScope: "public"})

	p, _ := h.registry.Create(NewPrompt{
		TurnID: "turn-x", Reason: "reason", Channel: "telegram",
		TTL: time.Minute, RaisedFor: "tg-@alice",
	})

	tapPrompt(t, h, p.ID, `{"id": 1, "username": "alice"}`)

	snap, _ := h.registry.Get(p.ID)
	if snap.Decision != PromptApproved {
		t.Fatalf("the right user could not answer: %s", snap.Decision)
	}
}

// Fails CLOSED. A prompt with no recorded audience cannot be
// attributed to anyone, and guessing in favour of the tapper is the
// wrong way to be wrong about who approved something.
func TestAPromptWithNoAudienceIsNotAnswerable(t *testing.T) {
	t.Parallel()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{UnknownUserScope: "public"})

	p, _ := h.registry.Create(NewPrompt{
		TurnID: "turn-x", Reason: "reason", Channel: "telegram", TTL: time.Minute,
	})

	tapPrompt(t, h, p.ID, `{"id": 1, "username": "alice"}`)

	snap, _ := h.registry.Get(p.ID)
	if snap.Decision != PromptPending {
		t.Fatalf("a prompt nobody was asked got answered: %s", snap.Decision)
	}

	// And it says the RIGHT thing. The principal check below it would
	// refuse this too — no audience matches nobody — but it would say
	// "not for you", which sends an operator looking for the wrong
	// problem. A prompt with no audience is a broken RAISE path, and
	// the message has to point there.
	if !sentTextContaining(h, "cannot be attributed") {
		t.Error("an unattributable prompt was refused as though it belonged to somebody else")
	}
}

// sentTextContaining reports whether any sendMessage carried the
// substring.
func sentTextContaining(h *tgPromptHarness, want string) bool {
	for _, c := range h.capturedCalls() {
		if c.Method != "sendMessage" {
			continue
		}
		if text, _ := c.Body["text"].(string); strings.Contains(text, want) {
			return true
		}
	}
	return false
}

// The refusal reaches the person who tapped. Silence reads as a broken
// button and invites retrying — after which they keep tapping and the
// person who can actually answer never learns there is a question
// waiting.
func TestARefusedTapIsToldWhy(t *testing.T) {
	t.Parallel()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{UnknownUserScope: "public"})

	p, _ := h.registry.Create(NewPrompt{
		TurnID: "turn-x", Reason: "reason", Channel: "telegram",
		TTL: time.Minute, RaisedFor: "tg-@alice",
	})

	tapPrompt(t, h, p.ID, `{"id": 2, "username": "mallory"}`)

	if !sentTextContaining(h, "not for you") {
		t.Error("the refused tap got no explanation, so the button just looks broken")
	}
}

// Every raise site must record an audience, or the guard above turns
// working confirmations into dead buttons. This is the inventory
// check: sendConfirmationKeyboard is the only path Telegram uses.
func TestTheTelegramRaisePathRecordsAnAudience(t *testing.T) {
	t.Parallel()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{UnknownUserScope: "public"})

	raiseConfirmation(t, h, "tool:exec", "/usr/bin/rg")

	ids := h.registry.pendingIDs()
	if len(ids) != 1 {
		t.Fatalf("expected one pending prompt, got %d", len(ids))
	}
	p, _ := h.registry.Get(ids[0])
	if p.RaisedFor == "" {
		t.Error("the real raise path produced a prompt nobody can answer")
	}
}

// pendingIDs lists the registry's live prompt ids. Test-only: the
// production paths always know the id they just created.
func (r *PromptRegistry) pendingIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.prompts))
	for id := range r.prompts {
		out = append(out, id)
	}
	return out
}
