package memory

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func taskStore(t *testing.T) *TaskApprovalStore {
	t.Helper()
	node, fsm := newTestRaft(t)
	s, e := NewTaskApprovalStore(node, fsm.Store())
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func createTask(t *testing.T, s *TaskApprovalStore, parent string) *pb.TaskApprovalRecord {
	t.Helper()
	r, e := s.CreateTaskApproval(context.Background(), &pb.CreateTaskApprovalRequest{Owner: "user:alice", Actor: "user:alice", ParentId: parent})
	if e != nil {
		t.Fatal(e)
	}
	return r.Record
}
func pauseTask(t *testing.T, s *TaskApprovalStore, r *pb.TaskApprovalRecord) *pb.TaskApprovalRecord {
	t.Helper()
	q := &pb.PauseTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Actor: r.Actor, Revision: r.Revision, ClaimToken: r.ClaimedBy, TurnId: "turn-1", Operation: &pb.TaskOperation{CallId: "call-1", ToolName: "shell_command", Action: "shell:run", Resource: "!unclassified", Labels: []string{"reads"}}, Continuation: &pb.Continuation{Messages: []*pb.SessionMessage{{Role: "tool", PreparedToolCall: &pb.PreparedToolCall{CallId: "call-1", ToolName: "shell_command", TurnId: "turn-1", Params: map[string]string{"command": "pwd && ls"}}}}}}
	out, e := s.PauseTaskApproval(context.Background(), q)
	if e != nil {
		t.Fatal(e)
	}
	return out.Record
}
func approveTask(t *testing.T, s *TaskApprovalStore, r *pb.TaskApprovalRecord) *pb.TaskApprovalRecord {
	t.Helper()
	out, e := s.DecideTaskApproval(context.Background(), &pb.DecideTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Revision: r.Revision, Choice: pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_RISK_LABELS})
	if e != nil {
		t.Fatal(e)
	}
	return out.Record
}
func claimApprovalTask(t *testing.T, s *TaskApprovalStore, r *pb.TaskApprovalRecord) *pb.TaskApprovalRecord {
	t.Helper()
	out, e := s.ClaimTaskApproval(context.Background(), &pb.ClaimTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Actor: r.Actor, Revision: r.Revision})
	if e != nil {
		t.Fatal(e)
	}
	return out.Record
}
func checkTaskGrant(s *TaskApprovalStore, r *pb.TaskApprovalRecord) (bool, error) {
	out, e := s.CheckGrantTaskApproval(context.Background(), &pb.CheckGrantTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Actor: r.Actor, ClaimToken: r.ClaimedBy, Action: "shell:run", Resource: "(risk=reads)"})
	if e != nil {
		return false, e
	}
	return out.Granted, nil
}

func TestTaskApprovalsDurableIsolatedAndRevocable(t *testing.T) {
	s := taskStore(t)
	r := claimApprovalTask(t, s, approveTask(t, s, pauseTask(t, s, createTask(t, s, ""))))
	// A fresh service reads only replicated state, with no local approval cache.
	other, e := NewTaskApprovalStore(s.raft, s.store)
	if e != nil {
		t.Fatal(e)
	}
	if ok, e := checkTaskGrant(other, r); e != nil || !ok {
		t.Fatalf("durable grant: %v %v", ok, e)
	}
	child := createTask(t, s, r.Id)
	if ok, e := checkTaskGrant(other, child); e != nil || ok {
		t.Fatalf("child inherited authority: %v %v", ok, e)
	}
	if _, e = s.GetTaskApproval(context.Background(), &pb.GetTaskApprovalRequest{Id: r.Id, Owner: "user:bob"}); status.Code(e) != codes.NotFound {
		t.Fatalf("owner leaked: %v", e)
	}
	listing, e := s.ListTaskApproval(context.Background(), &pb.ListTaskApprovalRequest{Owner: r.Owner})
	if e != nil {
		t.Fatal(e)
	}
	for _, item := range listing.Records {
		if item.Continuation != nil || item.ClaimedBy != "" || len(item.Grants) > 0 {
			t.Fatal("listing exposes execution authority")
		}
	}
	_, e = s.CancelTaskApproval(context.Background(), &pb.CancelTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Revision: r.Revision})
	if e != nil {
		t.Fatal(e)
	}
	if ok, e := checkTaskGrant(other, r); e == nil || ok {
		t.Fatal("cancelled grant survived on another reader")
	}
}
func TestTaskApprovalsConcurrentClaimHasOneWinner(t *testing.T) {
	s := taskStore(t)
	r := approveTask(t, s, pauseTask(t, s, createTask(t, s, "")))
	var wg sync.WaitGroup
	results := make(chan error, 10)
	for range 10 {
		wg.Go(func() {
			_, e := s.ClaimTaskApproval(context.Background(), &pb.ClaimTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Actor: r.Actor, Revision: r.Revision})
			results <- e
		})
	}
	wg.Wait()
	close(results)
	wins := 0
	for e := range results {
		if e == nil {
			wins++
		} else if status.Code(e) != codes.Aborted {
			t.Errorf("unexpected claim result: %v", e)
		}
	}
	if wins != 1 {
		t.Fatalf("claim winners %d", wins)
	}
}
func TestTaskApprovalsCrashNeedsExplicitRecovery(t *testing.T) {
	s := taskStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	r := claimApprovalTask(t, s, approveTask(t, s, pauseTask(t, s, createTask(t, s, ""))))
	now = now.Add(TaskExecutionTTL + time.Second)
	out, e := s.GetTaskApproval(context.Background(), &pb.GetTaskApprovalRequest{Id: r.Id, Owner: r.Owner})
	if e != nil {
		t.Fatal(e)
	}
	if out.Record.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_OUTCOME_UNKNOWN {
		t.Fatal("lost execution was not marked uncertain")
	}
	if _, e = s.ClaimTaskApproval(context.Background(), &pb.ClaimTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Actor: r.Actor, Revision: r.Revision}); status.Code(e) != codes.FailedPrecondition {
		t.Fatalf("automatic replay allowed: %v", e)
	}
	if _, e = s.RecoverTaskApproval(context.Background(), &pb.RecoverTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Revision: r.Revision}); status.Code(e) != codes.InvalidArgument {
		t.Fatal("recovery did not require acknowledgement")
	}
	recovered, e := s.RecoverTaskApproval(context.Background(), &pb.RecoverTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Revision: r.Revision, AcknowledgeDuplicateRisk: true})
	if e != nil {
		t.Fatal(e)
	}
	if recovered.Record.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_WAITING {
		t.Fatal("recovery did not require fresh approval")
	}
	if ok, e := checkTaskGrant(s, r); ok || e == nil {
		t.Fatal("old execution token survived recovery")
	}
	newRun := claimApprovalTask(t, s, approveTask(t, s, recovered.Record))
	if newRun.ClaimedBy == r.ClaimedBy {
		t.Fatal("execution token reused")
	}
}
func TestTaskApprovalsExpiryEnforcedBeforeSweep(t *testing.T) {
	s := taskStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	r := pauseTask(t, s, createTask(t, s, ""))
	now = now.Add(TaskApprovalTTL + time.Second)
	if _, e := s.DecideTaskApproval(context.Background(), &pb.DecideTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Revision: r.Revision, Choice: pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_ONCE}); status.Code(e) != codes.FailedPrecondition {
		t.Fatalf("expired approval accepted: %v", e)
	}
}
func TestTaskApprovalsRefuseBroadUnclassifiedGrant(t *testing.T) {
	s := taskStore(t)
	r := pauseTask(t, s, createTask(t, s, ""))
	if _, e := s.DecideTaskApproval(context.Background(), &pb.DecideTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Revision: r.Revision, Choice: pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_OPERATION}); status.Code(e) != codes.InvalidArgument {
		t.Fatal("unclassified reusable operation accepted")
	}
}

type failingTaskLog struct {
	inner raftApplier
	fail  bool
}

func (f *failingTaskLog) Apply(data []byte, timeout time.Duration) (any, error) {
	if f.fail {
		return nil, context.DeadlineExceeded
	}
	return f.inner.Apply(data, timeout)
}
func TestTaskApprovalsFailedDecisionDoesNotLeaveGrant(t *testing.T) {
	s := taskStore(t)
	r := pauseTask(t, s, createTask(t, s, ""))
	log := &failingTaskLog{inner: s.raft, fail: true}
	s.raft = log
	_, e := s.DecideTaskApproval(context.Background(), &pb.DecideTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Revision: r.Revision, Choice: pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_RISK_LABELS})
	if status.Code(e) != codes.Unavailable {
		t.Fatalf("write failure: %v", e)
	}
	stored, e := s.load(r.Id, r.Owner)
	if e != nil {
		t.Fatal(e)
	}
	if stored.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_WAITING || len(stored.Grants) != 0 {
		t.Fatal("failed decision changed authority")
	}
}
func TestTaskApprovalsBudgetWorkRemainsDiscoverable(t *testing.T) {
	s := taskStore(t)
	r := createTask(t, s, "")
	out, e := s.PauseTaskApproval(context.Background(), &pb.PauseTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Actor: r.Actor, Revision: r.Revision, ClaimToken: r.ClaimedBy, TurnId: "budget-turn", Operation: &pb.TaskOperation{RequiresBudgetExtension: true, Summary: "tool-call allowance exhausted"}, Continuation: &pb.Continuation{Messages: []*pb.SessionMessage{{Role: "assistant", Content: "work so far"}}, ToolCalls: 30}})
	if e != nil {
		t.Fatal(e)
	}
	if out.Record.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_WAITING {
		t.Fatal("budget work was not paused")
	}
	_, e = s.DecideTaskApproval(context.Background(), &pb.DecideTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Revision: out.Record.Revision, Choice: pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_ONCE})
	if status.Code(e) != codes.FailedPrecondition {
		t.Fatal("budget implicitly relaxed")
	}
	stored, e := s.load(r.Id, r.Owner)
	if e != nil {
		t.Fatal(e)
	}
	if stored.Continuation.ToolCalls != 30 {
		t.Fatal("spent budget lost")
	}
}
func TestTaskApprovalsDecisionRaceAndPagination(t *testing.T) {
	s := taskStore(t)
	r := pauseTask(t, s, createTask(t, s, ""))
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, choice := range []pb.TaskApprovalChoice{pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_ONCE, pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_DENY} {
		wg.Go(func() {
			_, e := s.DecideTaskApproval(context.Background(), &pb.DecideTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Revision: r.Revision, Choice: choice})
			results <- e
		})
	}
	wg.Wait()
	close(results)
	wins := 0
	for e := range results {
		if e == nil {
			wins++
		} else if status.Code(e) != codes.Aborted {
			t.Fatal(e)
		}
	}
	if wins != 1 {
		t.Fatalf("decisions won=%d", wins)
	}
	createTask(t, s, "")
	createTask(t, s, "")
	first, e := s.ListTaskApproval(context.Background(), &pb.ListTaskApprovalRequest{Owner: r.Owner, Limit: 2})
	if e != nil {
		t.Fatal(e)
	}
	if len(first.Records) != 2 || first.NextAfterId == "" {
		t.Fatal("missing page boundary")
	}
	next, e := s.ListTaskApproval(context.Background(), &pb.ListTaskApprovalRequest{Owner: r.Owner, Limit: 2, AfterId: first.NextAfterId})
	if e != nil {
		t.Fatal(e)
	}
	if len(next.Records) != 1 || next.NextAfterId != "" {
		t.Fatal("pagination repeated or lost record")
	}
}
