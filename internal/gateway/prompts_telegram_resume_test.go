package gateway

import (
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// A turn that needs approving twice must still be approvable the
// second time.
//
// The resumed leg builds its own SessionRef, and the second
// confirmation stamps RaisedFor from it. Built without the UserID,
// that prompt belongs to nobody: mayResolve fails closed, the person
// who raised the turn taps Approve and is told the confirmation
// "cannot be attributed to anyone", and a turn already approved once
// has no ending but its TTL.
func TestTelegramResumedTurnRaisesAnAnswerableConfirmation(t *testing.T) {
	t.Parallel()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{UnknownUserScope: "public"})

	// The session a resumed leg runs in, derived the way resume
	// derives it — from the prompt being resumed.
	session := resumeSessionForTelegram(&Prompt{
		ID:        "p-1",
		SessionID: "555",
		ChannelID: "555",
		RaisedFor: "u-7",
	})
	if session.UserID != "u-7" {
		t.Fatalf("resumed session UserID = %q; want the user the first question was asked of", session.UserID)
	}

	budget, _ := compute.NewTurnBudget(compute.BudgetCaps{})
	h.handler.sendConfirmationKeyboard(
		555,
		compute.ProcessMessageRequest{TurnID: "turn-42", Budget: budget},
		&compute.ProcessMessageResponse{ConfirmationReason: "and now this one?"},
		session,
	)

	calls := h.capturedCalls()
	if len(calls) != 1 {
		t.Fatalf("want 1 sendMessage; got %+v", calls)
	}
	markup, _ := calls[0].Body["reply_markup"].(map[string]any)
	rows, _ := markup["inline_keyboard"].([]any)
	if len(rows) == 0 {
		t.Fatalf("no keyboard on the second confirmation: %+v", calls[0].Body)
	}
	firstRow, _ := rows[0].([]any)
	btn, _ := firstRow[0].(map[string]any)
	cd, _ := btn["callback_data"].(string)
	parts := strings.SplitN(cd, ":", 3)
	if len(parts) != 3 {
		t.Fatalf("callback_data shape: %q", cd)
	}

	p, err := h.registry.Get(parts[2])
	if err != nil {
		t.Fatalf("registry doesn't know the second prompt: %v", err)
	}
	if p.RaisedFor != "u-7" {
		t.Fatalf("second confirmation RaisedFor = %q; want u-7 — an unattributed prompt is one nobody can approve", p.RaisedFor)
	}
}
