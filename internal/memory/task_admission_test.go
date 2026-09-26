package memory

import (
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestTaskAdmissionRejectsCapacityWithoutOrphanRecords(t *testing.T) {
	t.Parallel()
	raft, fsm := newTestRaft(t)
	ctx := t.Context()
	bots := NewBotService(raft, fsm.store)
	if _, err := bots.Put(ctx, &pb.BotRecord{Id: "worker", Owner: "user:alice", Enabled: true}, 0); err != nil {
		t.Fatal(err)
	}
	tasks, err := NewTaskApprovalStore(raft, fsm.store)
	if err != nil {
		t.Fatal(err)
	}
	create := func() (*pb.CreateTaskApprovalResponse, error) {
		return tasks.CreateTaskApproval(ctx, &pb.CreateTaskApprovalRequest{Owner: "user:alice", Actor: "bot:worker", InboxMaxPending: 1, Inbox: &pb.BotInboxItem{Recipient: "worker", Body: "work"}})
	}
	first, err := create()
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 12 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := create(); status.Code(err) != codes.ResourceExhausted {
				t.Errorf("capacity: %v", err)
			}
		}()
	}
	workers.Wait()
	count := 0
	if err := fsm.store.ForEach(BucketTaskApprovals, func(string, []byte) error { count++; return nil }); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("rejected admissions left %d tasks", count)
	}
	// Owner cancellation during execution is uncertain, but immediately frees
	// capacity without replay, even before inbox reconciliation runs.
	closed, err := tasks.CancelTaskApproval(ctx, &pb.CancelTaskApprovalRequest{Id: first.Record.Id, Owner: first.Record.Owner, Revision: first.Record.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if closed.Record.Recoverable {
		t.Fatal("cancelled execution advertised a nonexistent checkpoint")
	}
	if _, err := create(); err != nil {
		t.Fatalf("uncertainty retained queue capacity: %v", err)
	}
	final, err := tasks.CancelTaskApproval(ctx, &pb.CancelTaskApprovalRequest{Id: closed.Record.Id, Owner: closed.Record.Owner, Revision: closed.Record.Revision})
	if err != nil || final.Record.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_CANCELLED {
		t.Fatalf("explicit closure: %v %v", final, err)
	}
	if _, err := tasks.RecoverTaskApproval(ctx, &pb.RecoverTaskApprovalRequest{Id: final.Record.Id, Owner: final.Record.Owner, Revision: final.Record.Revision, AcknowledgeDuplicateRisk: true}); err == nil {
		t.Fatal("closed task could replay")
	}
}

func TestUncertainCheckpointRetainsRecoveryUntilExplicitClosure(t *testing.T) {
	t.Parallel()
	raft, fsm := newTestRaft(t)
	s, err := NewTaskApprovalStore(raft, fsm.store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	created, err := s.CreateTaskApproval(ctx, &pb.CreateTaskApprovalRequest{Owner: "user:alice", Actor: "user:alice"})
	if err != nil {
		t.Fatal(err)
	}
	r := created.Record
	paused, err := s.PauseTaskApproval(ctx, &pb.PauseTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Actor: r.Actor, Revision: r.Revision, ClaimToken: r.ClaimedBy, TurnId: "turn", Operation: &pb.TaskOperation{RequiresBudgetExtension: true}, Continuation: &pb.Continuation{Messages: []*pb.SessionMessage{{Role: "user", Content: "task"}}, ToolCalls: 2}, BudgetPolicy: &pb.TaskBudget{ToolCalls: 1}, BudgetLimits: &pb.TaskBudget{ToolCalls: 1}})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := s.DecideTaskApproval(ctx, &pb.DecideTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Revision: paused.Record.Revision, Choice: pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_BUDGET_EXTENSION, ExtraBudget: &pb.TaskBudget{ToolCalls: 2}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimTaskApproval(ctx, &pb.ClaimTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Actor: r.Actor, Revision: ready.Record.Revision})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(TaskExecutionTTL + time.Second)
	s.now = func() time.Time { return now }
	unknown, err := s.GetTaskApproval(ctx, &pb.GetTaskApprovalRequest{Id: r.Id, Owner: r.Owner})
	if err != nil {
		t.Fatal(err)
	}
	if !unknown.Record.Recoverable {
		t.Fatal("lost recoverable checkpoint")
	}
	recovered, err := s.RecoverTaskApproval(ctx, &pb.RecoverTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Revision: claimed.Record.Revision, AcknowledgeDuplicateRisk: true})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Record.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_WAITING {
		t.Fatal("recovery did not require fresh approval")
	}
}
