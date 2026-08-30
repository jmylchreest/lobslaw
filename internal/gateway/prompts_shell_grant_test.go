package gateway

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// What a shell approval grants, and what the user is told it granted.
//
// A grant used to cover a tool, and the prompt above it had just named
// that tool, so a reply reading "I won't ask about this again" was
// merely terse. A grant now covers a COMMAND, and the difference
// between `git status --short` and every command on the machine is the
// whole design — so a reply that does not name what it granted is
// asking the user to take the narrow reading on trust.
//
// The other half is the commands that cannot be granted at all.
// Compound commands have no stable form to remember, so the executor
// reports them grantable=false while still naming the real resource —
// policy has to match on it, and so does the turn approval. These tests
// pin both halves, because either failure is silent: a button that
// mints nothing looks exactly like one that worked, and a blanked
// resource looks fine right up until approving once loops forever.

func tapAlways(t *testing.T, h *tgPromptHarness, updateID, promptID string) {
	t.Helper()
	update := `{
		"update_id": ` + updateID + `,
		"callback_query": {
			"id": "cb-` + updateID + `",
			"from": {"id": 1, "username": "alice"},
			"message": {"message_id": 2, "chat": {"id": 99, "type": "private"}, "date": 0},
			"data": "prompt:approve-always:` + promptID + `"
		}
	}`
	if rec := postUpdate(t, h.handler, "test-webhook-secret", update); rec.Code != http.StatusOK {
		t.Fatalf("callback rejected: %d", rec.Code)
	}
}

// The rule minted names the exact command, not a class of them. A key
// that generalised over the tail would have made "ssh host uptime"
// grant "ssh host <anything>".
func TestAlwaysOnAShellCommandMintsAnExactRule(t *testing.T) {
	t.Parallel()
	rules, _ := newApprovalRulesForGateway(t)
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{
		UnknownUserScope: "public",
		Approvals:        compute.NewSessionApprovals(),
		ApprovalRules:    rules,
	})

	const command = "git status --short"
	raiseConfirmation(t, h, compute.ShellAction, command)

	promptID := callbackDataFor(t, h.capturedCalls(), "approve-always")
	if promptID == "" {
		t.Fatal("Always button carried no prompt id")
	}
	tapAlways(t, h, "910", promptID)

	minted, err := rules.FromApprovals()
	if err != nil {
		t.Fatal(err)
	}
	if len(minted) != 1 {
		t.Fatalf("%d rules minted, want 1: %+v", len(minted), minted)
	}
	rule := minted[0]
	if rule.Resource != command {
		t.Errorf("resource = %q, want the exact command %q", rule.Resource, command)
	}
	if rule.Action != compute.ShellAction {
		t.Errorf("action = %q, want %q", rule.Action, compute.ShellAction)
	}
	if strings.Contains(rule.Resource, "*") {
		t.Errorf("resource = %q; an approval must never carry a wildcard", rule.Resource)
	}
}

// The reply names the command. Otherwise the user has no way to tell a
// grant for one command from a grant for all of them, which is the
// only thing they needed to know.
func TestTheApprovalReplyNamesWhatWasGranted(t *testing.T) {
	t.Parallel()
	rules, _ := newApprovalRulesForGateway(t)
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{
		UnknownUserScope: "public",
		Approvals:        compute.NewSessionApprovals(),
		ApprovalRules:    rules,
	})

	const command = "git status --short"
	raiseConfirmation(t, h, compute.ShellAction, command)
	promptID := callbackDataFor(t, h.capturedCalls(), "approve-always")
	tapAlways(t, h, "911", promptID)

	replies := sendMessageTexts(t, h.capturedCalls())
	if !anyContains(replies, strings.ToLower(command)) {
		t.Errorf("the reply does not name what was granted; replies were %q", replies)
	}
	if !anyContains(replies, "revoke") {
		t.Errorf("the reply does not say how to undo it; replies were %q", replies)
	}
}

// An ungrantable command gets Approve and Deny only. Offering a scope
// button that mints something matching nothing would be worse than
// offering none.
func TestACommandWithNoStableFormOffersNoScopeButtons(t *testing.T) {
	t.Parallel()
	rules, _ := newApprovalRulesForGateway(t)
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{
		UnknownUserScope: "public",
		Approvals:        compute.NewSessionApprovals(),
		ApprovalRules:    rules,
	})

	// What the executor reports for `git status && rm -rf ~`: a real
	// resource, so policy and the turn approval can both match on it,
	// and grantable=false, so nothing is offered to remember.
	raiseConfirmationScoped(t, h, compute.ShellAction, compute.UnclassifiedResource, false)

	texts := buttonTexts(t, h.capturedCalls())
	for _, text := range texts {
		lower := strings.ToLower(text)
		if strings.Contains(lower, "always") || strings.Contains(lower, "chat") {
			t.Errorf("a scope button was offered for an ungrantable command: %v", texts)
		}
	}
	if len(texts) != 2 {
		t.Errorf("buttons = %v, want exactly Approve and Deny", texts)
	}
}

// The floor is checked at mint time as well as at execution time, so a
// rule listing never shows an operator a grant that reads as though it
// works.
func TestAlwaysCannotMintACommandTheFloorDenies(t *testing.T) {
	t.Parallel()
	rules, _ := newApprovalRulesForGateway(t)
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{
		UnknownUserScope: "public",
		Approvals:        compute.NewSessionApprovals(),
		ApprovalRules:    rules,
	})

	raiseConfirmation(t, h, compute.ShellAction, "rm -rf /")
	promptID := callbackDataFor(t, h.capturedCalls(), "approve-always")
	if promptID == "" {
		t.Fatal("Always button carried no prompt id")
	}
	tapAlways(t, h, "912", promptID)

	minted, err := rules.FromApprovals()
	if err != nil {
		t.Fatal(err)
	}
	if len(minted) != 0 {
		t.Errorf("the floor was reached by an approval: %+v", minted)
	}
	replies := sendMessageTexts(t, h.capturedCalls())
	if anyContains(replies, "won't ask") {
		t.Errorf("promised a grant that was never recorded; replies were %q", replies)
	}
}
