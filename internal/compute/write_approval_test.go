package compute

import (
	"context"
	"errors"
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

// Memory that silently self-modifies and cannot be inspected is a
// trust problem. `lobslaw memory list` answers "what happened"; it
// does not answer "may this happen", and the write lands either way.
//
// The gate is a POLICY question rather than a branch inside the tool,
// which is what makes the answer reusable — a session grant covers the
// conversation, an "always" mints a revocable rule, and an operator
// wanting something narrower writes an ordinary rule that outranks the
// config default.

// Exercised against a REAL policy engine rather than a stub. The gate
// is a policy question, so a stub would assert the plumbing and none
// of the behaviour that matters — which rule wins, and whether a
// session grant satisfies it.

func gatedExecutor(t *testing.T, rules ...*lobslawv1.PolicyRule) (*Executor, *SessionApprovals) {
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
	eng.SetDefaults([]types.PolicyRule{MemoryWriteApprovalDefault()})

	approvals := NewSessionApprovals()
	e := NewExecutor(newTestCatalogue(), eng, nil, ExecutorConfig{}, slog.New(slog.DiscardHandler))
	e.SetSessionApprovals(approvals)
	e.RequireApproval("memory_write", "episodic", MemoryWriteSummary)
	return e, approvals
}

func writeParams() map[string]string {
	return map[string]string{"event": "john prefers terse replies", "tags": `["preference"]`}
}

// The default: a write the operator has not spoken about is staged.
func TestAnUnapprovedWriteIsStaged(t *testing.T) {
	t.Parallel()
	e, _ := gatedExecutor(t)

	err := e.CheckGate(context.Background(), &types.Claims{UserID: "alice"},
		"memory_write", writeParams())
	if !errors.Is(err, ErrRequireConfirm) {
		t.Fatalf("err = %v, want ErrRequireConfirm", err)
	}
}

// The prompt has to say WHAT is being written. A confirmation reading
// only "the agent wants to write a memory" is one nobody can answer
// usefully, so they answer it reflexively — worse than not asking.
func TestThePromptSaysWhatIsBeingWritten(t *testing.T) {
	t.Parallel()
	e, _ := gatedExecutor(t)

	err := e.CheckGate(context.Background(), &types.Claims{UserID: "alice"},
		"memory_write", writeParams())
	if err == nil {
		t.Fatal("expected a confirmation")
	}
	if !strings.Contains(err.Error(), "john prefers terse replies") {
		t.Errorf("err = %q; it does not say what is being written", err)
	}
	if !strings.Contains(err.Error(), "preference") {
		t.Errorf("err = %q; the tags are missing", err)
	}
}

// A deny is a deny. Attaching the content to it would put the write in
// front of somebody who is not being asked to decide about it.
func TestADeniedWriteCarriesNoContent(t *testing.T) {
	t.Parallel()
	e, _ := gatedExecutor(t, &lobslawv1.PolicyRule{
		Id: "operator-forbids", Subject: "*", Action: MemoryWriteAction, Resource: "*",
		Effect: "deny", Priority: 0,
	})

	err := e.CheckGate(context.Background(), &types.Claims{UserID: "alice"},
		"memory_write", writeParams())
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("err = %v, want ErrPolicyDenied", err)
	}
	if strings.Contains(err.Error(), "john prefers terse replies") {
		t.Errorf("a denial carried the content: %q", err)
	}
}

// "Always" is an allow rule, so the gate simply passes — there is no
// separate always-store to consult, and building one would have meant
// two things deciding the same question.
func TestAnAlwaysAllowRulePassesTheGate(t *testing.T) {
	t.Parallel()
	e, _ := gatedExecutor(t, &lobslawv1.PolicyRule{
		Id: "approval:prompt-1", Subject: "user:alice",
		Action: MemoryWriteAction, Resource: "episodic",
		Effect: "allow", Priority: 1,
	})

	if err := e.CheckGate(context.Background(), &types.Claims{UserID: "alice"},
		"memory_write", writeParams()); err != nil {
		t.Errorf("an always-approved write was staged again: %v", err)
	}
}

// "For this conversation" goes through the session grants, which is
// the whole reason the gate is a policy question.
func TestASessionGrantSatisfiesTheGate(t *testing.T) {
	t.Parallel()
	e, approvals := gatedExecutor(t)
	ctx := turn.WithIdentity(context.Background(), turn.Identity{
		Principal: identity.Principal("user:alice"), Channel: "telegram", ChannelID: "42",
	})

	if err := e.CheckGate(ctx, &types.Claims{UserID: "alice"},
		"memory_write", writeParams()); !errors.Is(err, ErrRequireConfirm) {
		t.Fatalf("the first write was not staged: %v", err)
	}
	approvals.Grant(ctx, MemoryWriteAction, "episodic")
	if err := e.CheckGate(ctx, &types.Claims{UserID: "alice"},
		"memory_write", writeParams()); err != nil {
		t.Errorf("a granted conversation was asked again: %v", err)
	}
}

// A grant is scoped to the conversation it was given in, so another
// chat is still asked.
func TestAGrantDoesNotCoverAnotherConversation(t *testing.T) {
	t.Parallel()
	e, approvals := gatedExecutor(t)
	granted := turn.WithIdentity(context.Background(), turn.Identity{
		Principal: identity.Principal("user:alice"), Channel: "telegram", ChannelID: "42",
	})
	other := turn.WithIdentity(context.Background(), turn.Identity{
		Principal: identity.Principal("user:alice"), Channel: "telegram", ChannelID: "99",
	})
	approvals.Grant(granted, MemoryWriteAction, "episodic")

	if err := e.CheckGate(other, &types.Claims{UserID: "alice"},
		"memory_write", writeParams()); !errors.Is(err, ErrRequireConfirm) {
		t.Errorf("a grant leaked to another conversation: %v", err)
	}
}

// --- absence and scope ----------------------------------------------

// Off means no extra check at all, not a check that always passes.
func TestAnUngatedToolIsNotChecked(t *testing.T) {
	t.Parallel()
	// No policy engine at all: if the gate consulted one for an
	// unregistered tool this would fail with ErrNoPolicyEngine rather
	// than passing, which is a sharper assertion than counting calls.
	e := NewExecutor(newTestCatalogue(), nil, nil, ExecutorConfig{}, slog.New(slog.DiscardHandler))

	if err := e.CheckGate(context.Background(), &types.Claims{UserID: "alice"},
		"memory_write", writeParams()); err != nil {
		t.Fatalf("an ungated tool was checked: %v", err)
	}
}

// Only the marked tool is gated. A gate that caught everything would
// be a different feature and a much worse one.
func TestOnlyTheMarkedToolIsGated(t *testing.T) {
	t.Parallel()
	e, _ := gatedExecutor(t)
	if err := e.CheckGate(context.Background(), &types.Claims{UserID: "alice"},
		"memory_search", map[string]string{"query": "x"}); err != nil {
		t.Errorf("an unmarked tool was gated: %v", err)
	}
}

// The gate uses its OWN action. Reusing tool:exec would mean the allow
// rule that lets memory_write run at all silently satisfied the gate.
func TestTheGateUsesItsOwnAction(t *testing.T) {
	t.Parallel()
	// An allow for tool:exec on memory_write is what every deployment
	// has — otherwise the tool could not run. If the gate reused that
	// action, this rule would silently satisfy it.
	e, _ := gatedExecutor(t, &lobslawv1.PolicyRule{
		Id: "allow-the-tool", Subject: "*", Action: "tool:exec", Resource: "memory_write",
		Effect: "allow", Priority: 0,
	})

	err := e.CheckGate(context.Background(), &types.Claims{UserID: "alice"},
		"memory_write", writeParams())
	if !errors.Is(err, ErrRequireConfirm) {
		t.Errorf("err = %v; the tool:exec allow satisfied the write gate", err)
	}
}

// --- the default rule -----------------------------------------------

// A default that could outrank an operator's rule would not be a
// default.
func TestTheDefaultRuleIsTheLowestPriority(t *testing.T) {
	t.Parallel()
	r := MemoryWriteApprovalDefault()
	if r.Priority >= 0 {
		t.Errorf("priority = %d; it must lose to anything an operator wrote", r.Priority)
	}
	if r.Effect != types.EffectRequireConfirmation {
		t.Errorf("effect = %v", r.Effect)
	}
	if r.Action != MemoryWriteAction {
		t.Errorf("action = %q", r.Action)
	}
	// The provenance says where it came from, so an operator seeing it
	// in a listing is not left wondering who wrote it.
	if !strings.Contains(r.ID, "memory.write_approval") {
		t.Errorf("id = %q; it does not name the setting that created it", r.ID)
	}
}

// --- the summary ------------------------------------------------------

func TestTheSummaryIsTruncated(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 5000)
	got := MemoryWriteSummary(map[string]string{"event": long})
	if len([]rune(got)) > 250 {
		t.Errorf("summary is %d runes; a three-screen prompt is one nobody reads", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated summary does not say it was truncated: %q", got)
	}
}

func TestAnEmptyEventHasNoSummary(t *testing.T) {
	t.Parallel()
	if got := MemoryWriteSummary(map[string]string{}); got != "" {
		t.Errorf("got %q", got)
	}
}
