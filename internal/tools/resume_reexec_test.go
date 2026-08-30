package tools

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Approving a confirmation has to RUN THE THING.
//
// When a turn pauses, its last two messages are the assistant's tool
// call and a tool-role result saying "tool invocation requires
// confirmation". Resuming replayed exactly that into the loop, so the
// first thing the model saw on resume was its own call having been
// refused — and a model that has just been told it was refused does not
// try again. It explains the refusal instead.
//
// The user therefore tapped Approve and got a paragraph about needing
// approval, and every retry raised a fresh prompt. The approval was
// recorded correctly, the policy gate would have passed, and none of
// that mattered because the call was never re-issued.
//
// Approving names an operation. Resuming re-runs THAT operation and
// replaces the refusal with its real result, rather than asking the
// model to decide again from a transcript that says no.

func resumeAgent(t *testing.T) *compute.Agent {
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

	rule := &lobslawv1.PolicyRule{
		Id: "lobslaw-builtin-shell_command", Subject: "*",
		Action: "tool:exec", Resource: "shell_command",
		Effect: "allow", Priority: 1,
	}
	raw, err := proto.Marshal(rule)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(memory.BucketPolicyRules, rule.Id, raw); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.DiscardHandler)
	eng := policy.NewEngine(store, log)
	eng.SetDefaults([]types.PolicyRule{compute.ShellApprovalDefault()})

	reg := NewRegistry()
	if err := reg.Register(ShellToolDef()); err != nil {
		t.Fatal(err)
	}
	builtins := NewBuiltins()
	if err := RegisterShellBuiltin(builtins); err != nil {
		t.Fatal(err)
	}

	e := compute.NewExecutor(reg, eng, nil, compute.ExecutorConfig{}, log)
	e.SetBuiltins(builtins)
	e.SetSessionApprovals(compute.NewSessionApprovals())
	e.RequireCommandApproval("shell_command", compute.ShellGrantResource, compute.ShellCommandSummary)

	// One tool call, then a text reply once the model has a result.
	mock := compute.NewMockProvider(
		compute.MockResponse{ToolCalls: []compute.ToolCall{{
			ID: "call-1", Name: "shell_command",
			Arguments: `{"command":"echo hello-from-the-shell"}`,
		}}},
		compute.MockResponse{Content: "done"},
		compute.MockResponse{Content: "done"},
	)
	a, err := compute.NewAgent(compute.AgentConfig{Provider: mock, Executor: e, Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func resumeReq(t *testing.T) compute.ProcessMessageRequest {
	t.Helper()
	return compute.ProcessMessageRequest{
		Message:   "run echo for me",
		Claims:    &types.Claims{UserID: "alice"},
		TurnID:    "turn-1",
		Channel:   "telegram",
		ChannelID: "42",
		Budget:    mkBudget(t, compute.BudgetCaps{}),
	}
}

func TestApprovingAConfirmationActuallyRunsTheCommand(t *testing.T) {
	t.Parallel()
	a := resumeAgent(t)
	ctx := turn.WithIdentity(context.Background(), turn.Identity{
		Principal: identity.Principal("user:alice"), Channel: "telegram", ChannelID: "42",
	})
	req := resumeReq(t)

	resp, err := a.RunToolCallLoop(ctx, req)
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	if !resp.NeedsConfirmation {
		t.Fatalf("the shell call was not staged for confirmation; reply = %q", resp.Reply)
	}

	// Exactly what the channel does: relax the budget, carry the
	// answer, replay the messages the turn stopped on.
	req.Budget.Relax()
	approved := compute.WithTurnApproval(ctx, resp.ConfirmationAction, resp.ConfirmationResource)
	resumed, err := a.ResumeFromConfirmation(approved, req, resp.Messages)
	if err != nil {
		t.Fatalf("ResumeFromConfirmation: %v", err)
	}

	// The command must actually have run. Its output is the proof —
	// a resume that only re-asked the model would produce prose about
	// needing approval and no tool output anywhere.
	var ran bool
	for _, m := range resumed.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "hello-from-the-shell") {
			ran = true
		}
	}
	if !ran {
		t.Errorf("the approved command never ran; messages = %s", messageTrace(resumed.Messages))
	}
	if resumed.NeedsConfirmation {
		t.Errorf("the resumed turn asked for confirmation again: %q", resumed.ConfirmationReason)
	}
}

// The refusal must not survive into the resumed transcript either. A
// model that can still see "requires confirmation" in its history will
// keep explaining the refusal even when the call has since succeeded.
func TestTheRefusalIsReplacedByTheRealResult(t *testing.T) {
	t.Parallel()
	a := resumeAgent(t)
	ctx := turn.WithIdentity(context.Background(), turn.Identity{
		Principal: identity.Principal("user:alice"), Channel: "telegram", ChannelID: "42",
	})
	req := resumeReq(t)

	resp, err := a.RunToolCallLoop(ctx, req)
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	if !resp.NeedsConfirmation {
		t.Fatal("no confirmation was raised")
	}

	req.Budget.Relax()
	approved := compute.WithTurnApproval(ctx, resp.ConfirmationAction, resp.ConfirmationResource)
	resumed, err := a.ResumeFromConfirmation(approved, req, resp.Messages)
	if err != nil {
		t.Fatalf("ResumeFromConfirmation: %v", err)
	}

	for _, m := range resumed.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, compute.ErrRequireConfirm.Error()) {
			t.Errorf("the refusal is still in the transcript the model reads:\n%s",
				messageTrace(resumed.Messages))
			break
		}
	}
}

func messageTrace(msgs []compute.Message) string {
	var b strings.Builder
	for i, m := range msgs {
		b.WriteString("  [")
		b.WriteString(m.Role)
		b.WriteString("] ")
		content := m.Content
		if len(content) > 160 {
			content = content[:160] + "…"
		}
		b.WriteString(strings.ReplaceAll(content, "\n", " "))
		if len(m.ToolCalls) > 0 {
			b.WriteString(" (tool_calls: ")
			for _, tc := range m.ToolCalls {
				b.WriteString(tc.Name)
			}
			b.WriteString(")")
		}
		if i < len(msgs)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// Approving a SPEND must not re-run anything.
//
// The resume re-execution finds the approved call by scanning back for
// a tool message carrying the require-confirmation sentinel. A budget
// pause also resumes, and its transcript tail is the last SUCCESSFUL
// tool result — so a file or web page containing that phrase could
// steer the scan onto a completed call and have it run a second time.
// Non-idempotent calls (a message send, a memory write, a push) would
// fire twice, and the real result would be overwritten in the
// transcript.
//
// The gate is the turn approval: a budget confirmation carries no
// operation, so it never sets one, and the re-execution never runs.
func TestABudgetApprovalReRunsNothing(t *testing.T) {
	t.Parallel()
	a := resumeAgent(t)
	ctx := turn.WithIdentity(context.Background(), turn.Identity{
		Principal: identity.Principal("user:alice"), Channel: "telegram", ChannelID: "42",
	})
	req := resumeReq(t)

	// A transcript whose last tool message is a SUCCESS whose content
	// happens to contain the sentinel — exactly what reading a file
	// that discusses this feature would produce.
	poisoned := []compute.Message{
		{Role: "user", Content: "summarise the notes"},
		{Role: "assistant", ToolCalls: []compute.ToolCall{{
			ID: "call-9", Name: "shell_command",
			Arguments: `{"command":"echo poisoned-should-not-rerun"}`,
		}}},
		{
			Role:       "tool",
			ToolCallID: "call-9",
			Content: "<untrusted source=\"tool:shell_command:output\">the docs say " +
				compute.ErrRequireConfirm.Error() + " when a rule matches</untrusted>",
		},
	}

	req.Budget.Relax()
	// No turn approval: this is what a budget confirmation resumes with.
	resumed, err := a.ResumeFromConfirmation(ctx, req, poisoned)
	if err != nil {
		t.Fatalf("ResumeFromConfirmation: %v", err)
	}
	for _, m := range resumed.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "poisoned-should-not-rerun") {
			t.Fatalf("untrusted tool output caused a re-execution:\n%s", messageTrace(resumed.Messages))
		}
	}
	// The model is free to call tools of its own accord on resume —
	// that is an ordinary turn continuing. What must not happen is the
	// RESUME PATH re-running the completed call behind its back.
	for _, inv := range resumed.ToolCalls {
		if inv.CallID == "call-9" {
			t.Errorf("the completed call was re-executed: %+v", inv)
		}
	}
}
