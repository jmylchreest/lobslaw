package compute

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// A confirmation has to tell the channel which operation it is about.
//
// It used to report tool:exec and the tool's own name no matter which
// gate raised it, and the memory:write gate asks under its own action.
// So the answer was recorded against an operation nobody consults:
// "approve for this chat" granted tool:exec/memory_write, which the
// gate never reads, and "always allow" minted an allow rule for
// tool:exec/memory_write that wire_seeds.go already seeds on every
// node. Both reported success and neither changed anything, so the
// same prompt came back on the very next write, forever.
//
// The unit tests missed it because they called the gate directly and
// did the granting by hand. Nothing walked the path the channel
// actually takes: Invoke → runToolCall → pendingConfirmation.

func gatedAgent(t *testing.T, rules ...*lobslawv1.PolicyRule) (*Agent, *Executor, *SessionApprovals) {
	t.Helper()
	dir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(dir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The tool:exec allow every node carries from wire_seeds.go. Its
	// presence is the whole point: the gate must not be satisfied by
	// it, and an "always" that minted it again would grant nothing.
	rules = append(rules, &lobslawv1.PolicyRule{
		Id: "lobslaw-builtin-memory_write", Subject: "*",
		Action: "tool:exec", Resource: "memory_write",
		Effect: "allow", Priority: 1,
	})
	for _, r := range rules {
		raw, err := proto.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(memory.BucketPolicyRules, r.Id, raw); err != nil {
			t.Fatal(err)
		}
	}

	eng := policy.NewEngine(store, slog.New(slog.DiscardHandler))
	eng.SetDefaults([]types.PolicyRule{WriteApprovalDefault()})

	reg := newTestCatalogue()
	if err := reg.Register(&types.ToolDef{
		Name:     "memory_write",
		Path:     BuiltinScheme + "memory_write",
		RiskTier: types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}

	approvals := NewSessionApprovals()
	e := NewExecutor(reg, eng, nil, ExecutorConfig{}, slog.New(slog.DiscardHandler))
	e.SetSessionApprovals(approvals)
	e.RequireApproval("memory_write", "episodic", MemoryWriteSummary)

	a, err := NewAgent(AgentConfig{Provider: NewMockProvider(), Executor: e})
	if err != nil {
		t.Fatal(err)
	}
	return a, e, approvals
}

func confirmRequest(t *testing.T) ProcessMessageRequest {
	t.Helper()
	return ProcessMessageRequest{
		Claims: &types.Claims{UserID: "alice"},
		TurnID: "turn-1",
		Budget: mkBudget(t, BudgetCaps{}),
	}
}

func writeCall() ToolCall {
	return ToolCall{
		ID:        "call-1",
		Name:      "memory_write",
		Arguments: `{"event":"john prefers terse replies","tags":"[\"preference\"]"}`,
	}
}

// The gate asks under memory:write, so that is what the channel has to
// be told. Reporting tool:exec sends the answer somewhere nothing reads.
func TestAGatedConfirmationCarriesTheGatesOwnAction(t *testing.T) {
	t.Parallel()
	a, _, _ := gatedAgent(t)

	_, pending, err := a.runToolCall(context.Background(), confirmRequest(t), writeCall())
	if err != nil {
		t.Fatalf("runToolCall: %v", err)
	}
	if pending == nil {
		t.Fatal("the write was not staged for confirmation")
	}
	if pending.Action != ApprovalAction {
		t.Errorf("Action = %q, want %q", pending.Action, ApprovalAction)
	}
	if pending.Resource != "episodic" {
		t.Errorf("Resource = %q, want %q", pending.Resource, "episodic")
	}
}

// The prompt says what is being written, not which rule fired. The
// rule's reason explains why an approval is wanted; only the summary
// says what is about to happen, and only that is answerable.
func TestAGatedConfirmationSaysWhatIsBeingWritten(t *testing.T) {
	t.Parallel()
	a, _, _ := gatedAgent(t)

	_, pending, err := a.runToolCall(context.Background(), confirmRequest(t), writeCall())
	if err != nil {
		t.Fatalf("runToolCall: %v", err)
	}
	if pending == nil {
		t.Fatal("the write was not staged for confirmation")
	}
	if want := "john prefers terse replies"; !strings.Contains(pending.Reason, want) {
		t.Errorf("Reason = %q; it does not say what is being written", pending.Reason)
	}
}

// The other case, and the reason the fallback stays: when the tool:exec
// check itself asked, an operator wrote a rule about the tool, and a
// grant about the tool is what they were asking to be able to give.
func TestAToolLevelConfirmationStillCarriesToolExec(t *testing.T) {
	t.Parallel()
	// Outranks the priority-1 allow, so the tool:exec check is the one
	// that asks — before the gate is ever reached.
	a, _, _ := gatedAgent(t, &lobslawv1.PolicyRule{
		Id: "operator-asks-about-the-tool", Subject: "*",
		Action: "tool:exec", Resource: "memory_write",
		Effect: "require_confirmation", Priority: 20,
	})

	_, pending, err := a.runToolCall(context.Background(), confirmRequest(t), writeCall())
	if err != nil {
		t.Fatalf("runToolCall: %v", err)
	}
	if pending == nil {
		t.Fatal("the call was not staged for confirmation")
	}
	if pending.Action != "tool:exec" {
		t.Errorf("Action = %q, want tool:exec", pending.Action)
	}
	if pending.Resource != "memory_write" {
		t.Errorf("Resource = %q, want memory_write", pending.Resource)
	}
}

// The property the whole thing exists for: an answer recorded against
// what the confirmation REPORTED must satisfy the gate that raised it.
// This is what failed before — the grant landed on tool:exec and the
// gate went on asking about memory:write.
func TestAGrantIsHonouredAfterwards(t *testing.T) {
	t.Parallel()
	a, _, approvals := gatedAgent(t)
	ctx := turn.WithIdentity(context.Background(), turn.Identity{
		Principal: identity.Principal("user:alice"), Channel: "telegram", ChannelID: "42",
	})

	_, pending, err := a.runToolCall(ctx, confirmRequest(t), writeCall())
	if err != nil {
		t.Fatalf("runToolCall: %v", err)
	}
	if pending == nil {
		t.Fatal("the write was not staged for confirmation")
	}

	// Exactly what the channel does with a "for this chat" tap: it
	// knows nothing but what the confirmation told it.
	approvals.Grant(ctx, pending.Action, pending.Resource)

	_, pending, err = a.runToolCall(ctx, confirmRequest(t), writeCall())
	if err != nil {
		t.Fatalf("runToolCall after the grant: %v", err)
	}
	if pending != nil {
		t.Errorf("asked again after the user answered: action=%q resource=%q",
			pending.Action, pending.Resource)
	}
}
