package gateway

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// "Always" is the button that makes a confirmation stop coming back
// forever. It is also the one that can quietly widen what the agent is
// allowed to do, so both halves matter: that it works, and that the
// user is told when it did not.

func newApprovalRulesForGateway(t *testing.T) (*policy.ApprovalRules, *memory.Store) {
	t.Helper()
	dataDir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(dataDir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	_, inmem := raft.NewInmemTransport("always-node")
	node, err := memory.NewRaft(memory.RaftConfig{
		NodeID: "always-node", LocalAddr: "always-node",
		DataDir: dataDir, Bootstrap: true, Transport: inmem,
	}, memory.NewFSM(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = store.Close()
	})
	if err := node.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	rules, err := policy.NewApprovalRules(node, store)
	if err != nil {
		t.Fatal(err)
	}
	return rules, store
}

// raiseConfirmation drives the keyboard path directly, as the other
// prompt tests do — the agent loop that produces a confirmation is
// tested elsewhere and would only add noise here.
func raiseConfirmation(t *testing.T, h *tgPromptHarness, action, resource string) {
	t.Helper()
	raiseConfirmationScoped(t, h, action, resource, true)
}

// raiseConfirmationScoped is the same with grantable spelled out, for
// the operations whose answer cannot be remembered.
func raiseConfirmationScoped(t *testing.T, h *tgPromptHarness, action, resource string, grantable bool) {
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
			ConfirmationReason:    "do the thing?",
			ConfirmationAction:    action,
			ConfirmationResource:  resource,
			ConfirmationGrantable: grantable,
		},
		SessionRef{Channel: "telegram", ChannelID: "99", UserID: "tg-@alice"},
	)
}

// sendMessageTexts returns every message the bot sent. The approval
// reply is not the last one — the resumed turn speaks after it — so
// assertions look across all of them rather than at the tail.
func sendMessageTexts(t *testing.T, calls []tgAPICall) []string {
	t.Helper()
	var out []string
	for _, c := range calls {
		if c.Method != "sendMessage" {
			continue
		}
		if text, ok := c.Body["text"].(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func anyContains(texts []string, want string) bool {
	for _, s := range texts {
		if strings.Contains(strings.ToLower(s), want) {
			return true
		}
	}
	return false
}

// buttonTexts pulls the inline-keyboard labels out of the captured
// sendMessage call.
func buttonTexts(t *testing.T, calls []tgAPICall) []string {
	t.Helper()
	for _, c := range calls {
		if c.Method != "sendMessage" {
			continue
		}
		markup, ok := c.Body["reply_markup"].(map[string]any)
		if !ok {
			continue
		}
		rows, ok := markup["inline_keyboard"].([]any)
		if !ok {
			continue
		}
		var out []string
		for _, row := range rows {
			cells, _ := row.([]any)
			for _, cell := range cells {
				b, _ := cell.(map[string]any)
				if text, ok := b["text"].(string); ok {
					out = append(out, text)
				}
			}
		}
		return out
	}
	return nil
}

func callbackDataFor(t *testing.T, calls []tgAPICall, verb string) string {
	t.Helper()
	for _, c := range calls {
		if c.Method != "sendMessage" {
			continue
		}
		markup, _ := c.Body["reply_markup"].(map[string]any)
		rows, _ := markup["inline_keyboard"].([]any)
		for _, row := range rows {
			cells, _ := row.([]any)
			for _, cell := range cells {
				b, _ := cell.(map[string]any)
				data, _ := b["callback_data"].(string)
				if after, ok := strings.CutPrefix(data, "prompt:"+verb+":"); ok {
					return after
				}
			}
		}
	}
	return ""
}

// A node with nowhere to record a permanent grant must not offer the
// button. Showing one that silently does nothing is worse than not
// having it.
func TestAlwaysButtonHiddenWithoutSomewhereToRecordIt(t *testing.T) {
	t.Parallel()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{
		UnknownUserScope: "public",
		Approvals:        compute.NewSessionApprovals(),
		// No ApprovalRules.
	})

	raiseConfirmation(t, h, "tool:exec", "write_file")

	texts := buttonTexts(t, h.capturedCalls())
	for _, text := range texts {
		if strings.Contains(strings.ToLower(text), "always") {
			t.Errorf("offered %q with no ApprovalRules wired; buttons were %v", text, texts)
		}
	}
}

func TestAlwaysButtonMintsARevocableRule(t *testing.T) {
	t.Parallel()
	rules, _ := newApprovalRulesForGateway(t)
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{
		UnknownUserScope: "public",
		Approvals:        compute.NewSessionApprovals(),
		ApprovalRules:    rules,
	})

	raiseConfirmation(t, h, "tool:exec", "write_file")

	texts := buttonTexts(t, h.capturedCalls())
	var sawAlways bool
	for _, text := range texts {
		if strings.Contains(strings.ToLower(text), "always") {
			sawAlways = true
		}
	}
	if !sawAlways {
		t.Fatalf("no Always button offered; buttons were %v", texts)
	}

	promptID := callbackDataFor(t, h.capturedCalls(), "approve-always")
	if promptID == "" {
		t.Fatal("Always button carried no prompt id")
	}

	update := `{
		"update_id": 900,
		"callback_query": {
			"id": "cb-always",
			"from": {"id": 1, "username": "alice"},
			"message": {"message_id": 2, "chat": {"id": 99, "type": "private"}, "date": 0},
			"data": "prompt:approve-always:` + promptID + `"
		}
	}`
	if rec := postUpdate(t, h.handler, "test-webhook-secret", update); rec.Code != http.StatusOK {
		t.Fatalf("callback rejected: %d", rec.Code)
	}

	snap, err := h.registry.Get(promptID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Decision != PromptApproved {
		t.Errorf("decision = %s, want approved", snap.Decision)
	}

	minted, err := rules.FromApprovals()
	if err != nil {
		t.Fatal(err)
	}
	if len(minted) != 1 {
		t.Fatalf("%d rules minted, want 1: %+v", len(minted), minted)
	}
	rule := minted[0]
	if rule.Effect != "allow" {
		t.Errorf("effect = %q, want allow", rule.Effect)
	}
	if rule.Subject != "user:tg-@alice" {
		t.Errorf("subject = %q, want the principal from the turn's claims", rule.Subject)
	}
	if rule.CreatedBy != "approval:"+promptID {
		t.Errorf("created_by = %q; without provenance the grant is not revocable as a class",
			rule.CreatedBy)
	}

	// The user is told it stuck, and told how to undo it.
	replies := sendMessageTexts(t, h.capturedCalls())
	if !anyContains(replies, "won't ask") {
		t.Errorf("nothing said the grant is permanent; replies were %q", replies)
	}
	if !anyContains(replies, "revoke") {
		t.Errorf("nothing said how to undo it; replies were %q", replies)
	}
}

// A mint the floor refuses must not be reported as a permanent grant.
// Telling the user "I won't ask again" when the rule was never written
// means they find out by being asked again.
func TestRefusedMintDoesNotClaimSuccess(t *testing.T) {
	t.Parallel()
	rules, _ := newApprovalRulesForGateway(t)
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{
		UnknownUserScope: "public",
		Approvals:        compute.NewSessionApprovals(),
		ApprovalRules:    rules,
	})

	raiseConfirmation(t, h, "tool:exec", "/etc/shadow")
	promptID := callbackDataFor(t, h.capturedCalls(), "approve-always")
	if promptID == "" {
		t.Fatal("Always button carried no prompt id")
	}

	update := `{
		"update_id": 901,
		"callback_query": {
			"id": "cb-always-refused",
			"from": {"id": 1, "username": "alice"},
			"message": {"message_id": 2, "chat": {"id": 99, "type": "private"}, "date": 0},
			"data": "prompt:approve-always:` + promptID + `"
		}
	}`
	_ = postUpdate(t, h.handler, "test-webhook-secret", update)

	minted, err := rules.FromApprovals()
	if err != nil {
		t.Fatal(err)
	}
	if len(minted) != 0 {
		t.Errorf("the floor was reached by an approval: %+v", minted)
	}

	replies := sendMessageTexts(t, h.capturedCalls())
	if anyContains(replies, "won't ask") {
		t.Errorf("promised a permanent grant that was never recorded; replies were %q", replies)
	}
}
