package gateway

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// The button that exists because the other buttons cannot help.
//
// An agent probing its environment writes a different compound command
// every time, so a per-command grant is never matched twice and no
// grant can name the command at all — the user is offered exactly one
// answer, Approve, over and over, which is how a confirmation becomes a
// reflex. A tier is nameable even when the command is not.
//
// The failure modes here are silent in both directions: a button that
// records nothing looks exactly like one that worked, and a button
// offered on a destructive command grants far more than anybody read.

func raiseRiskConfirmation(t *testing.T, h *tgPromptHarness, resource string, grantable bool, labels []compute.RiskLabel) {
	t.Helper()
	budget, err := compute.NewTurnBudget(compute.BudgetCaps{})
	if err != nil {
		t.Fatal(err)
	}
	h.handler.sendConfirmationKeyboard(
		99,
		compute.ProcessMessageRequest{
			TurnID: "turn-1",
			Budget: budget,
			Claims: &types.Claims{UserID: "tg-@alice"},
		},
		&compute.ProcessMessageResponse{
			ConfirmationReason:    "run the thing?",
			ConfirmationAction:    compute.ShellAction,
			ConfirmationResource:  resource,
			ConfirmationGrantable: grantable,
			ConfirmationLabels:    labels,
		},
		SessionRef{Channel: "telegram", ChannelID: "99", UserID: "tg-@alice"},
	)
}

func tapRiskGrant(t *testing.T, h *tgPromptHarness, updateID, promptID string) {
	t.Helper()
	update := `{
		"update_id": ` + updateID + `,
		"callback_query": {
			"id": "cb-` + updateID + `",
			"from": {"id": 1, "username": "alice"},
			"message": {"message_id": 2, "chat": {"id": 99, "type": "private"}, "date": 0},
			"data": "prompt:approve-session-risk:` + promptID + `"
		}
	}`
	if rec := postUpdate(t, h.handler, "test-webhook-secret", update); rec.Code != http.StatusOK {
		t.Fatalf("callback rejected: %d", rec.Code)
	}
}

// The case the whole button exists for: a compound probe, which cannot
// be granted per command, still offers the tier.
func TestTheTierButtonIsOfferedWhenTheCommandCannotBeGranted(t *testing.T) {
	t.Parallel()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{
		UnknownUserScope: "public",
		Approvals:        compute.NewSessionApprovals(),
	})

	raiseRiskConfirmation(t, h, compute.UnclassifiedResource, false, compute.L(compute.LabelReads))

	calls := h.capturedCalls()
	if got := callbackDataFor(t, calls, "approve-session-risk"); got == "" {
		t.Error("no tier button on a command that cannot be granted per command")
	}
	// The per-command buttons stay hidden: nothing could be minted.
	if got := callbackDataFor(t, calls, "approve-session"); got != "" {
		t.Error("a per-command session button was offered for an ungrantable command")
	}
}

// Tapping it records a grant covering the TIER, not the command — so
// the next command, a different one, is not asked about either.
func TestTappingTheTierButtonGrantsTheTier(t *testing.T) {
	t.Parallel()
	approvals := compute.NewSessionApprovals()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{
		UnknownUserScope: "public",
		Approvals:        approvals,
	})

	raiseRiskConfirmation(t, h, compute.UnclassifiedResource, false, compute.L(compute.LabelReads))
	promptID := callbackDataFor(t, h.capturedCalls(), "approve-session-risk")
	if promptID == "" {
		t.Fatal("no tier button")
	}
	tapRiskGrant(t, h, "920", promptID)

	ctx := turn.WithIdentity(t.Context(), turn.Identity{Channel: "telegram", ChannelID: "99"})
	if !approvals.Granted(ctx, compute.ShellAction, compute.RiskGrantResource(compute.LabelReads)) {
		t.Error("no read-tier grant was recorded")
	}
	// Bounded by the tier it named.
	if approvals.Granted(ctx, compute.ShellAction, compute.RiskGrantResource(compute.LabelWrites)) {
		t.Error("a read grant also covered writes")
	}
	// And the reply says what was given, because this offer is broader
	// than every other button on the prompt.
	var said string
	for _, text := range sendMessageTexts(t, h.capturedCalls()) {
		if strings.Contains(text, "reads") {
			said = text
		}
	}
	if said == "" {
		t.Error("the reply did not name the tier that was granted")
	}
	if !strings.Contains(said, "still asks") {
		t.Errorf("the reply did not say what is still gated: %q", said)
	}
}

// Never for the tiers nobody should hand over with one tap in a chat
// window. If this starts passing, riskGrantOffered has grown a case it
// should not have.
func TestTheTierButtonIsNeverOfferedForTheDangerousTiers(t *testing.T) {
	t.Parallel()
	for _, labels := range [][]compute.RiskLabel{
		compute.L(compute.LabelNetwork), compute.L(compute.LabelDeletes),
		compute.L(compute.LabelUnreadable), nil,
		// A set with ANY ungrantable label offers nothing, even when the
		// rest would have been fine on their own.
		compute.L(compute.LabelReads, compute.LabelDeletes),
	} {
		t.Run(compute.RenderLabels(labels), func(t *testing.T) {
			h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{
				UnknownUserScope: "public",
				Approvals:        compute.NewSessionApprovals(),
			})
			raiseRiskConfirmation(t, h, "rm -rf /etc/hosts", true, labels)
			if got := callbackDataFor(t, h.capturedCalls(), "approve-session-risk"); got != "" {
				t.Errorf("a label button was offered for %v", labels)
			}
		})
	}
}

// The callback is attacker-shaped input and its id is guessable, so the
// tier is re-checked when the tap arrives rather than trusted from it.
func TestATappedTierGrantIsRecheckedAgainstTheTier(t *testing.T) {
	t.Parallel()
	approvals := compute.NewSessionApprovals()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{
		UnknownUserScope: "public",
		Approvals:        approvals,
	})

	// Grantable per command, so a prompt exists and carries a pending
	// scope — but the tier is destructive, so no tier button was
	// rendered. Tapping the verb anyway must record nothing.
	raiseRiskConfirmation(t, h, "rm -rf /etc/hosts", true, compute.L(compute.LabelDeletes))
	promptID := callbackDataFor(t, h.capturedCalls(), "approve-session")
	if promptID == "" {
		t.Fatal("no per-command session button to borrow a prompt id from")
	}
	tapRiskGrant(t, h, "930", promptID)

	ctx := turn.WithIdentity(t.Context(), turn.Identity{Channel: "telegram", ChannelID: "99"})
	if approvals.Granted(ctx, compute.ShellAction, compute.RiskGrantResource(compute.LabelDeletes)) {
		t.Fatal("a destructive tier was granted by a forged callback")
	}
}

func TestRiskGrantOffered(t *testing.T) {
	t.Parallel()
	cases := []struct {
		labels []compute.RiskLabel
		want   bool
	}{
		{compute.L(compute.LabelReads), true},
		{compute.L(compute.LabelWrites), true},
		{compute.L(compute.LabelReads, compute.LabelWrites), true},
		{compute.L(compute.LabelNetwork), false},
		{compute.L(compute.LabelDeletes), false},
		{compute.L(compute.LabelDisrupts), false},
		{compute.L(compute.LabelPrivilege), false},
		{compute.L(compute.LabelUnreadable), false},
		// EVERY label must be grantable. A command that reads and
		// deletes offers nothing, because approving "this kind of
		// thing" would approve the deletion.
		{compute.L(compute.LabelReads, compute.LabelDeletes), false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := riskGrantOffered(tc.labels); got != tc.want {
			t.Errorf("riskGrantOffered(%v) = %v, want %v", tc.labels, got, tc.want)
		}
		if tc.want && riskGrantLabel(tc.labels) == "" {
			t.Errorf("%v is offered but has no button label", tc.labels)
		}
		if !tc.want && riskGrantLabel(tc.labels) != "" {
			t.Errorf("%v is not offered but has a button label", tc.labels)
		}
	}
}

// Five buttons in one strip render on a phone as five slivers of
// truncated text, and telling "Approve for this chat" from "Allow
// read-only here" at a glance is the entire point of having both.
func TestKeyboardRows(t *testing.T) {
	t.Parallel()
	buttons := make([]map[string]string, 5)
	rows := keyboardRows(buttons)
	if len(rows) != 3 {
		t.Fatalf("%d rows for 5 buttons, want 3", len(rows))
	}
	total := 0
	for _, r := range rows {
		if len(r) > 2 {
			t.Errorf("a row holds %d buttons, want at most 2", len(r))
		}
		total += len(r)
	}
	if total != len(buttons) {
		t.Errorf("%d buttons laid out, want %d", total, len(buttons))
	}
	if len(keyboardRows(nil)) != 0 {
		t.Error("no buttons produced rows")
	}
}
