package compute

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Exercise the actual FSM and encrypted store without a second network cluster;
// memory's task tests separately cover real-Raft proposal races.
type taskTestLog struct {
	mu    sync.Mutex
	index uint64
	fsm   *memory.FSM
}

func (l *taskTestLog) Apply(data []byte, _ time.Duration) (any, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.index++
	return l.fsm.Apply(&raft.Log{Index: l.index, Type: raft.LogCommand, Data: data}), nil
}
func TestTaskRunnerResumesPreparedCallOnce(t *testing.T) {
	ctx := context.Background()
	key, e := crypto.GenerateKey()
	if e != nil {
		t.Fatal(e)
	}
	db, e := memory.OpenStore(filepath.Join(t.TempDir(), "state.db"), key)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = db.Close() })
	log := &taskTestLog{fsm: memory.NewFSM(db)}
	backend, e := memory.NewTaskApprovalStore(log, db)
	if e != nil {
		t.Fatal(e)
	}
	created, e := backend.CreateTaskApproval(ctx, &pb.CreateTaskApprovalRequest{Owner: "user:alice", Actor: "user:alice"})
	if e != nil {
		t.Fatal(e)
	}
	agent, executor, _ := gatedAgent(t)
	agent.cfg.Provider = NewMockProvider(MockResponse{Content: "finished"})
	calls := 0
	dispatcher := newTestDispatcher()
	if e = dispatcher.Register("memory_write", func(context.Context, map[string]string) ([]byte, int, error) { calls++; return []byte("saved"), 0, nil }); e != nil {
		t.Fatal(e)
	}
	executor.SetBuiltins(dispatcher)
	req := confirmRequest(t)
	tc := writeCall()
	inv, pending, e := agent.runToolCall(ctx, req, tc)
	if e != nil || pending == nil {
		t.Fatalf("expected initial confirmation: %v %v", pending, e)
	}
	response := &ProcessMessageResponse{NeedsConfirmation: true, ConfirmationReason: pending.Reason, ConfirmationAction: pending.Action, ConfirmationResource: pending.Resource, ConfirmationGrantable: pending.Grantable, Messages: []Message{{Role: "assistant", ToolCalls: []ToolCall{tc}}, toolResultMessage(tc, inv)}, BudgetState: req.Budget.State()}
	paused, e := PauseTask(ctx, backend, created.Record, req, response)
	if e != nil {
		t.Fatal(e)
	}
	approved, e := backend.DecideTaskApproval(ctx, &pb.DecideTaskApprovalRequest{Id: paused.Id, Owner: paused.Owner, Revision: paused.Revision, Choice: pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_ONCE})
	if e != nil {
		t.Fatal(e)
	}
	// New adapter simulates a different runner reading the saved checkpoint.
	backend, e = memory.NewTaskApprovalStore(log, db)
	if e != nil {
		t.Fatal(e)
	}
	runner := TaskRunner{Backend: backend, Agent: agent, Resolve: func(context.Context, string, string) (TaskAuthority, error) {
		return TaskAuthority{Claims: &types.Claims{UserID: "alice"}, Caps: BudgetCaps{MaxToolCalls: 10}}, nil
	}}
	claim := &pb.ClaimTaskApprovalRequest{Id: paused.Id, Owner: paused.Owner, Actor: paused.Actor, Revision: approved.Record.Revision}
	completed, e := runner.Resume(ctx, claim)
	if e != nil {
		t.Fatal(e)
	}
	if completed.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_COMPLETED || calls != 1 {
		t.Fatalf("resume state=%s calls=%d", completed.State, calls)
	}
	if _, e = runner.Resume(ctx, claim); e == nil || calls != 1 {
		t.Fatalf("duplicate resume ran: %v calls=%d", e, calls)
	}
}

func TestTaskRunnerBudgetExtensionKeepsConsumptionAndBound(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	backend, e := memory.NewTaskApprovalStore(&taskTestLog{fsm: memory.NewFSM(env.store)}, env.store)
	if e != nil {
		t.Fatal(e)
	}
	created, e := backend.CreateTaskApproval(ctx, &pb.CreateTaskApprovalRequest{Owner: "user:alice", Actor: "user:alice"})
	if e != nil {
		t.Fatal(e)
	}
	req := confirmRequest(t)
	req.Budget = mkBudget(t, BudgetCaps{MaxToolCalls: 3})
	req.Budget.Restore(BudgetState{ToolCalls: 4})
	paused, e := PauseTask(ctx, backend, created.Record, req, &ProcessMessageResponse{NeedsConfirmation: true, ConfirmationReason: "budget exceeded on tool_calls", Messages: []Message{{Role: "user", Content: "finish the task"}}, BudgetState: req.Budget.State()})
	if e != nil {
		t.Fatal(e)
	}
	approved, e := backend.DecideTaskApproval(ctx, &pb.DecideTaskApprovalRequest{Id: paused.Id, Owner: paused.Owner, Revision: paused.Revision, Choice: pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_BUDGET_EXTENSION, ExtraBudget: &pb.TaskBudget{ToolCalls: 2}})
	if e != nil {
		t.Fatal(e)
	}
	if approved.Record.BudgetLimits.ToolCalls != 6 || approved.Record.BudgetSpent.ToolCalls != 4 {
		t.Fatalf("extension lost bound or consumption: %v", approved.Record)
	}
	// The resumed model keeps asking for tools. Two calls fit; a third must pause
	// again rather than inheriting the chat path's unlimited Budget.Relax().
	calls := 0
	dispatcher := newTestDispatcher()
	if e = dispatcher.Register("echo", func(context.Context, map[string]string) ([]byte, int, error) { calls++; return []byte("ok"), 0, nil }); e != nil {
		t.Fatal(e)
	}
	env.executor.SetBuiltins(dispatcher)
	if e = env.reg.Register(&types.ToolDef{Name: "echo", Path: BuiltinScheme + "echo"}); e != nil {
		t.Fatal(e)
	}
	provider := NewMockProvider(MockResponse{ToolCalls: []ToolCall{{ID: "1", Name: "echo", Arguments: `{}`}}}, MockResponse{ToolCalls: []ToolCall{{ID: "2", Name: "echo", Arguments: `{}`}}}, MockResponse{ToolCalls: []ToolCall{{ID: "3", Name: "echo", Arguments: `{}`}}})
	agent, e := NewAgent(AgentConfig{Provider: provider, Executor: env.executor})
	if e != nil {
		t.Fatal(e)
	}
	runner := TaskRunner{Backend: backend, Agent: agent, Resolve: func(context.Context, string, string) (TaskAuthority, error) {
		return TaskAuthority{Claims: &types.Claims{UserID: "alice", Scope: "owner"}, Caps: BudgetCaps{MaxToolCalls: 3}}, nil
	}}
	next, e := runner.Resume(ctx, &pb.ClaimTaskApprovalRequest{Id: paused.Id, Owner: paused.Owner, Actor: paused.Actor, Revision: approved.Record.Revision})
	if e != nil {
		t.Fatal(e)
	}
	if calls != 2 || next.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_WAITING || !next.Operation.RequiresBudgetExtension {
		t.Fatalf("extension was not bounded: calls=%d next=%v", calls, next)
	}
	if next.BudgetSpent.ToolCalls != 7 || next.BudgetLimits.ToolCalls != 6 {
		t.Fatalf("budget was reset: %v", next)
	}
}

func TestTaskBudgetTightenedPolicyWins(t *testing.T) {
	record := &pb.TaskApprovalRecord{BudgetPolicy: &pb.TaskBudget{ToolCalls: 30, SpendUsd: 5, EgressBytes: 100}, BudgetLimits: &pb.TaskBudget{ToolCalls: 40, SpendUsd: 8, EgressBytes: 200}}
	current := BudgetCaps{MaxToolCalls: 10, MaxSpendUSD: 2, MaxEgressBytes: 50}
	if got := effectiveTaskBudget(current, record); got != current {
		t.Fatalf("old approval bypassed new limits: %+v", got)
	}
}

func TestTaskSkillCheckpointPreservesDispatchAndParameters(t *testing.T) {
	agent, _, _ := gatedAgent(t, &pb.PolicyRule{Id: "confirm-skill", Subject: "*", Action: "tool:exec", Resource: "research", Effect: "require_confirmation", Priority: 100})
	skills := &fakeSkillDispatcher{known: map[string]struct{}{"research": {}}}
	agent.cfg.Skills = skills
	agent.cfg.Provider = NewMockProvider(MockResponse{Content: "done"})
	req := confirmRequest(t)
	tc := ToolCall{ID: "skill-call", Name: "research", Arguments: `{"query":"suppliers"}`}
	inv, pending, e := agent.runToolCall(context.Background(), req, tc)
	if e != nil || pending == nil {
		t.Fatalf("skill did not pause: %v %v", pending, e)
	}
	if inv.prepared == nil || inv.prepared.DispatchKind != "skill" {
		t.Fatal("skill checkpoint missing dispatch binding")
	}
	messages := []Message{{Role: "assistant", ToolCalls: []ToolCall{tc}}, toolResultMessage(tc, inv)}
	wire := EncodeContinuation(&TaskContinuation{Request: req, Messages: messages})
	restored, e := DecodeContinuation(wire, BudgetCaps{})
	if e != nil {
		t.Fatal(e)
	}
	restored.Request.TurnID = req.TurnID
	_, e = agent.ResumeFromConfirmation(WithTurnApproval(context.Background(), pending.Action, pending.Resource), restored.Request, restored.Messages)
	if e != nil {
		t.Fatal(e)
	}
	if skills.calls != 1 || skills.lastReq.Params["query"] != "suppliers" {
		t.Fatalf("wrong resumed skill: %+v", skills)
	}
	// A deployment changing a skill name into an executable must not reuse the
	// approval obtained for the skill, even if it keeps the same argument schema.
	agent.cfg.Skills = nil
	if _, _, e = agent.runToolCallWithPrepared(context.Background(), req, tc, inv.prepared); e == nil {
		t.Fatal("skill approval moved to executor dispatch")
	}
}
