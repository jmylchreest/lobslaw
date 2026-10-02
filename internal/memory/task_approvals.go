package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/commandrisk"
	"github.com/jmylchreest/lobslaw/internal/ids"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const (
	// TaskApprovalTTL bounds authority even when a worker never reports completion.
	TaskApprovalTTL = 24 * time.Hour
	// TaskExecutionTTL bounds one execution leg. Long work must checkpoint before
	// this deadline; losing a leg never makes it eligible for automatic replay.
	TaskExecutionTTL  = 15 * time.Minute
	maxTaskCheckpoint = 2 << 20
	maxTaskGrants     = 128
)

// TaskApprovalStore is the channel-independent approval lifecycle. Its caller
// must authenticate owner/actor assertions (the node exposes it only to peers).
// Grants and continuation transitions share one CAS record, so a failed write
// cannot leave authority behind after a failed decision.
type TaskApprovalStore struct {
	raft  raftApplier
	store *Store
	now   func() time.Time
}

func NewTaskApprovalStore(raft raftApplier, store *Store) (*TaskApprovalStore, error) {
	if raft == nil || store == nil {
		return nil, errors.New("task approvals require raft and store")
	}
	return &TaskApprovalStore{raft: raft, store: store, now: time.Now}, nil
}

func validTaskIdentity(s string) bool {
	return s != "" && len(s) <= 256 && !strings.ContainsAny(s, "\x00\r\n")
}

func (s *TaskApprovalStore) load(id, owner string) (*pb.TaskApprovalRecord, error) {
	if !validTaskIdentity(owner) || id == "" {
		return nil, status.Error(codes.InvalidArgument, "task id and authenticated owner required")
	}
	raw, err := s.store.Get(BucketTaskApprovals, id)
	if err != nil {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	r := new(pb.TaskApprovalRecord)
	if err = proto.Unmarshal(raw, r); err != nil {
		return nil, status.Error(codes.DataLoss, "invalid task record")
	}
	if r.Owner != owner {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	s.effectiveState(r)
	return r, nil
}

// Read-time expiry is authoritative; it cannot be delayed by a sweeper or lost
// on restart. The next mutation persists the derived state with the same CAS.
func (s *TaskApprovalStore) effectiveState(r *pb.TaskApprovalRecord) {
	now := s.now()
	expired := r.ExpiresAt == nil || !now.Before(r.ExpiresAt.AsTime())
	switch r.State {
	case pb.TaskApprovalState_TASK_APPROVAL_STATE_RUNNING, pb.TaskApprovalState_TASK_APPROVAL_STATE_RESUMING:
		if expired || r.ClaimExpiresAt == nil || !now.Before(r.ClaimExpiresAt.AsTime()) {
			r.State = pb.TaskApprovalState_TASK_APPROVAL_STATE_OUTCOME_UNKNOWN
		}
	case pb.TaskApprovalState_TASK_APPROVAL_STATE_WAITING, pb.TaskApprovalState_TASK_APPROVAL_STATE_READY:
		if expired {
			r.State = pb.TaskApprovalState_TASK_APPROVAL_STATE_EXPIRED
		}
	}
}

func (s *TaskApprovalStore) write(ctx context.Context, previous, next *pb.TaskApprovalRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	revision := uint64(0)
	claimer := ""
	if previous != nil {
		revision = previous.Revision
		claimer = previous.ClaimedBy
	}
	data, err := proto.Marshal(&pb.LogEntry{Op: pb.LogOp_LOG_OP_CLAIM, Id: next.Id, ExpectedRevision: &revision, ExpectedClaimer: claimer, Payload: &pb.LogEntry_TaskApproval{TaskApproval: next}})
	if err != nil {
		return err
	}
	result, err := s.raft.Apply(data, 5*time.Second)
	if err == nil {
		if e, ok := result.(error); ok {
			err = e
		}
	}
	if errors.Is(err, ErrClaimConflict) {
		return status.Error(codes.Aborted, "task changed; refresh before deciding or resuming")
	}
	if err != nil {
		return status.Errorf(codes.Unavailable, "persist task: %v", err)
	}
	next.Revision = revision + 1
	return nil
}

func checkTaskRevision(r *pb.TaskApprovalRecord, revision uint64) error {
	if revision == 0 || revision != r.Revision {
		return status.Error(codes.Aborted, "task revision changed")
	}
	return nil
}
func checkTaskActor(r *pb.TaskApprovalRecord, actor string) error {
	if actor == "" || r.Actor != actor {
		return status.Error(codes.PermissionDenied, "task actor mismatch")
	}
	return nil
}
func checkTaskExecution(r *pb.TaskApprovalRecord, actor, token string) error {
	if err := checkTaskActor(r, actor); err != nil {
		return err
	}
	if (r.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_RUNNING && r.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_RESUMING) || token == "" || token != r.ClaimedBy {
		return status.Error(codes.FailedPrecondition, "task execution is no longer authorised")
	}
	return nil
}
func cloneTask(r *pb.TaskApprovalRecord) *pb.TaskApprovalRecord {
	return proto.Clone(r).(*pb.TaskApprovalRecord)
}
func taskMetadata(r *pb.TaskApprovalRecord) *pb.TaskApprovalRecord {
	r = cloneTask(r)
	r.Continuation = nil
	r.Grants = nil
	r.ClaimedBy = ""
	return r
}

func (s *TaskApprovalStore) CreateTaskApproval(ctx context.Context, q *pb.CreateTaskApprovalRequest) (*pb.CreateTaskApprovalResponse, error) {
	if q == nil || !validTaskIdentity(q.Owner) || !validTaskIdentity(q.Actor) {
		return nil, status.Error(codes.InvalidArgument, "owner and actor required")
	}
	now := s.now()
	until := now.Add(TaskApprovalTTL)
	if q.ExpiresAt != nil {
		if q.ExpiresAt.CheckValid() != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid task expiry")
		}
		until = q.ExpiresAt.AsTime()
	}
	if !until.After(now) || until.After(now.Add(TaskApprovalTTL)) {
		return nil, status.Error(codes.InvalidArgument, "task lifetime must be positive and at most 24 hours")
	}
	if q.ParentId != "" {
		if _, err := s.load(q.ParentId, q.Owner); err != nil {
			return nil, err
		}
	}
	r := &pb.TaskApprovalRecord{Id: ids.New(), Owner: q.Owner, Actor: q.Actor, ParentId: q.ParentId, State: pb.TaskApprovalState_TASK_APPROVAL_STATE_RUNNING, ExpiresAt: timestamppb.New(until), CreatedAt: timestamppb.New(now), ClaimedBy: ids.New(), ClaimExpiresAt: timestamppb.New(minTime(until, now.Add(TaskExecutionTTL)))}
	if err := s.write(ctx, nil, r); err != nil {
		return nil, err
	}
	return &pb.CreateTaskApprovalResponse{Record: r}, nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func validateTaskCheckpoint(q *pb.PauseTaskApprovalRequest) error {
	if q.Operation != nil && q.Operation.RequiresBudgetExtension {
		if q.Continuation == nil || len(q.Continuation.Messages) == 0 || q.TurnId == "" || proto.Size(q) > maxTaskCheckpoint {
			return status.Error(codes.InvalidArgument, "budget checkpoint requires bounded continuation and turn")
		}
		return nil
	}
	if q.Operation == nil || q.Continuation == nil || q.TurnId == "" || q.Operation.CallId == "" || q.Operation.ToolName == "" || q.Operation.Action == "" || q.Operation.Resource == "" {
		return status.Error(codes.InvalidArgument, "prepared operation, turn and continuation required")
	}
	if proto.Size(q) > maxTaskCheckpoint {
		return status.Error(codes.ResourceExhausted, "task checkpoint exceeds 2 MiB")
	}
	// Only server-prepared invocations may resume. This also prevents an older
	// runner silently dropping the hook output this approval is about.
	for _, m := range q.Continuation.Messages {
		p := m.PreparedToolCall
		if p != nil && p.CallId == q.Operation.CallId && p.ToolName == q.Operation.ToolName && p.TurnId == q.TurnId {
			return nil
		}
	}
	return status.Error(codes.InvalidArgument, "pending operation has no matching prepared invocation")
}
func (s *TaskApprovalStore) PauseTaskApproval(ctx context.Context, q *pb.PauseTaskApprovalRequest) (*pb.PauseTaskApprovalResponse, error) {
	if q == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	if q.Continuation != nil && !validTaskBudget(&pb.TaskBudget{ToolCalls: q.Continuation.ToolCalls, SpendUsd: q.Continuation.SpentUsd, EgressBytes: q.Continuation.EgressBytes}) {
		return nil, status.Error(codes.InvalidArgument, "invalid consumed budget")
	}
	if !validTaskBudget(q.BudgetPolicy) || !validTaskBudget(q.BudgetLimits) {
		return nil, status.Error(codes.InvalidArgument, "invalid budget checkpoint")
	}
	if err := validateTaskCheckpoint(q); err != nil {
		return nil, err
	}
	r, err := s.load(q.Id, q.Owner)
	if err != nil {
		return nil, err
	}
	if err = checkTaskRevision(r, q.Revision); err != nil {
		return nil, err
	}
	if err = checkTaskExecution(r, q.Actor, q.ClaimToken); err != nil {
		return nil, err
	}
	n := cloneTask(r)
	n.State = pb.TaskApprovalState_TASK_APPROVAL_STATE_WAITING
	n.Operation = proto.Clone(q.Operation).(*pb.TaskOperation)
	n.Continuation = proto.Clone(q.Continuation).(*pb.Continuation)
	n.TurnId = q.TurnId
	n.BudgetPolicy = q.BudgetPolicy
	n.BudgetLimits = q.BudgetLimits
	n.BudgetSpent = &pb.TaskBudget{ToolCalls: q.Continuation.ToolCalls, SpendUsd: q.Continuation.SpentUsd, EgressBytes: q.Continuation.EgressBytes}
	n.ClaimedBy = ""
	n.ClaimExpiresAt = nil
	n.DecidedBy = ""
	n.DecidedAt = nil
	if err = s.write(ctx, r, n); err != nil {
		return nil, err
	}
	return &pb.PauseTaskApprovalResponse{Record: taskMetadata(n)}, nil
}
func (s *TaskApprovalStore) GetTaskApproval(_ context.Context, q *pb.GetTaskApprovalRequest) (*pb.GetTaskApprovalResponse, error) {
	if q == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	r, e := s.load(q.Id, q.Owner)
	if e != nil {
		return nil, e
	}
	return &pb.GetTaskApprovalResponse{Record: taskMetadata(r)}, nil
}
func (s *TaskApprovalStore) ListTaskApproval(_ context.Context, q *pb.ListTaskApprovalRequest) (*pb.ListTaskApprovalResponse, error) {
	if q == nil || !validTaskIdentity(q.Owner) || q.Limit < 0 || q.Limit > 100 {
		return nil, status.Error(codes.InvalidArgument, "owner required; limit must be 0..100")
	}
	limit := int(q.Limit)
	if limit == 0 {
		limit = 50
	}
	out := &pb.ListTaskApprovalResponse{}
	stop := errors.New("page complete")
	err := s.store.ForEach(BucketTaskApprovals, func(id string, raw []byte) error {
		if id <= q.AfterId {
			return nil
		}
		r := new(pb.TaskApprovalRecord)
		if err := proto.Unmarshal(raw, r); err != nil {
			return fmt.Errorf("decode task: %w", err)
		}
		if r.Owner != q.Owner {
			return nil
		}
		if len(out.Records) == limit {
			out.NextAfterId = out.Records[len(out.Records)-1].Id
			return stop
		}
		s.effectiveState(r)
		out.Records = append(out.Records, taskMetadata(r))
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return nil, err
	}
	return out, nil
}
func (s *TaskApprovalStore) DecideTaskApproval(ctx context.Context, q *pb.DecideTaskApprovalRequest) (*pb.DecideTaskApprovalResponse, error) {
	if q == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	r, e := s.load(q.Id, q.Owner)
	if e != nil {
		return nil, e
	}
	if e = checkTaskRevision(r, q.Revision); e != nil {
		return nil, e
	}
	if q.ExtraBudget != nil && q.Choice != pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_BUDGET_EXTENSION {
		return nil, status.Error(codes.InvalidArgument, "extra budget requires a budget-extension decision")
	}
	if r.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_WAITING {
		return nil, status.Error(codes.FailedPrecondition, "task is not waiting for approval")
	}
	if r.Operation.RequiresBudgetExtension && q.Choice != pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_DENY && q.Choice != pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_BUDGET_EXTENSION {
		return nil, status.Error(codes.FailedPrecondition, "budget work is saved; an explicit budget extension is required")
	}
	n := cloneTask(r)
	n.State = pb.TaskApprovalState_TASK_APPROVAL_STATE_READY
	n.DecidedBy = q.Owner
	n.DecidedAt = timestamppb.New(s.now())
	switch q.Choice {
	case pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_BUDGET_EXTENSION:
		if !r.Operation.RequiresBudgetExtension {
			return nil, status.Error(codes.InvalidArgument, "this prompt is not a budget request")
		}
		limits, err := extendTaskBudget(r, q.ExtraBudget)
		if err != nil {
			return nil, err
		}
		n.BudgetLimits = limits
	case pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_ONCE:
	case pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_DENY:
		n.State = pb.TaskApprovalState_TASK_APPROVAL_STATE_DENIED
		n.Grants = nil
		n.Continuation = nil
	case pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_OPERATION:
		if !r.Operation.Grantable || strings.ContainsAny(r.Operation.Action+r.Operation.Resource, "*\x00") {
			return nil, status.Error(codes.InvalidArgument, "operation does not support a reusable grant")
		}
		addTaskGrant(n, r.Operation.Action, r.Operation.Resource)
	case pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_RISK_LABELS:
		if len(r.Operation.Labels) == 0 {
			return nil, status.Error(codes.InvalidArgument, "operation has no grantable labels")
		}
		for _, label := range r.Operation.Labels {
			if label != string(commandrisk.LabelReads) && label != string(commandrisk.LabelWrites) {
				return nil, status.Error(codes.InvalidArgument, "only reads and writes can be granted for a task")
			}
			addTaskGrant(n, r.Operation.Action, "(risk="+label+")")
		}
	default:
		return nil, status.Error(codes.InvalidArgument, "explicit approval choice required")
	}
	if len(n.Grants) > maxTaskGrants {
		return nil, status.Error(codes.ResourceExhausted, "task grant limit reached")
	}
	if e = s.write(ctx, r, n); e != nil {
		return nil, e
	}
	return &pb.DecideTaskApprovalResponse{Record: taskMetadata(n)}, nil
}
func addTaskGrant(r *pb.TaskApprovalRecord, action, resource string) {
	for _, g := range r.Grants {
		if g.Action == action && g.Resource == resource {
			return
		}
	}
	r.Grants = append(r.Grants, &pb.TaskGrant{Action: action, Resource: resource})
}
func (s *TaskApprovalStore) ClaimTaskApproval(ctx context.Context, q *pb.ClaimTaskApprovalRequest) (*pb.ClaimTaskApprovalResponse, error) {
	if q == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	r, e := s.load(q.Id, q.Owner)
	if e != nil {
		return nil, e
	}
	if e = checkTaskRevision(r, q.Revision); e != nil {
		return nil, e
	}
	if e = checkTaskActor(r, q.Actor); e != nil {
		return nil, e
	}
	if r.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_READY {
		return nil, status.Error(codes.FailedPrecondition, "task is not approved for resumption")
	}
	n := cloneTask(r)
	n.State = pb.TaskApprovalState_TASK_APPROVAL_STATE_RESUMING
	n.ClaimedBy = ids.New()
	n.ClaimExpiresAt = timestamppb.New(minTime(n.ExpiresAt.AsTime(), s.now().Add(TaskExecutionTTL)))
	if e = s.write(ctx, r, n); e != nil {
		return nil, e
	}
	return &pb.ClaimTaskApprovalResponse{Record: n}, nil
}
func (s *TaskApprovalStore) FinishTaskApproval(ctx context.Context, q *pb.FinishTaskApprovalRequest) (*pb.FinishTaskApprovalResponse, error) {
	if q == nil || len(q.Result) > 65536 {
		return nil, status.Error(codes.InvalidArgument, "result must be at most 64 KiB")
	}
	r, e := s.load(q.Id, q.Owner)
	if e != nil {
		return nil, e
	}
	if e = checkTaskRevision(r, q.Revision); e != nil {
		return nil, e
	}
	if e = checkTaskExecution(r, q.Actor, q.ClaimToken); e != nil {
		return nil, e
	}
	n := cloneTask(r)
	n.State = pb.TaskApprovalState_TASK_APPROVAL_STATE_COMPLETED
	n.Result = q.Result
	clearTaskAuthority(n)
	if e = s.write(ctx, r, n); e != nil {
		return nil, e
	}
	return &pb.FinishTaskApprovalResponse{Record: taskMetadata(n)}, nil
}
func clearTaskAuthority(r *pb.TaskApprovalRecord) {
	r.Grants = nil
	r.Continuation = nil
	r.ClaimedBy = ""
	r.ClaimExpiresAt = nil
}
func (s *TaskApprovalStore) CancelTaskApproval(ctx context.Context, q *pb.CancelTaskApprovalRequest) (*pb.CancelTaskApprovalResponse, error) {
	if q == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	r, e := s.load(q.Id, q.Owner)
	if e != nil {
		return nil, e
	}
	if e = checkTaskRevision(r, q.Revision); e != nil {
		return nil, e
	}
	switch r.State {
	case pb.TaskApprovalState_TASK_APPROVAL_STATE_COMPLETED, pb.TaskApprovalState_TASK_APPROVAL_STATE_DENIED, pb.TaskApprovalState_TASK_APPROVAL_STATE_CANCELLED, pb.TaskApprovalState_TASK_APPROVAL_STATE_EXPIRED:
		return nil, status.Error(codes.FailedPrecondition, "task is already closed")
	}
	n := cloneTask(r)
	switch n.State {
	case pb.TaskApprovalState_TASK_APPROVAL_STATE_RUNNING, pb.TaskApprovalState_TASK_APPROVAL_STATE_RESUMING, pb.TaskApprovalState_TASK_APPROVAL_STATE_OUTCOME_UNKNOWN:
		n.State = pb.TaskApprovalState_TASK_APPROVAL_STATE_OUTCOME_UNKNOWN
		n.Result = "Cancellation requested; an action may already have executed. No further execution is authorised."
	default:
		n.State = pb.TaskApprovalState_TASK_APPROVAL_STATE_CANCELLED
	}
	clearTaskAuthority(n)
	if e = s.write(ctx, r, n); e != nil {
		return nil, e
	}
	return &pb.CancelTaskApprovalResponse{Record: taskMetadata(n)}, nil
}
func (s *TaskApprovalStore) RecoverTaskApproval(ctx context.Context, q *pb.RecoverTaskApprovalRequest) (*pb.RecoverTaskApprovalResponse, error) {
	if q == nil || !q.AcknowledgeDuplicateRisk {
		return nil, status.Error(codes.InvalidArgument, "explicit acknowledgement of possible duplicate effects required")
	}
	r, e := s.load(q.Id, q.Owner)
	if e != nil {
		return nil, e
	}
	if e = checkTaskRevision(r, q.Revision); e != nil {
		return nil, e
	}
	if r.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_OUTCOME_UNKNOWN || r.Continuation == nil || !s.now().Before(r.ExpiresAt.AsTime()) {
		return nil, status.Error(codes.FailedPrecondition, "task has no recoverable checkpoint within its lifetime")
	}
	n := cloneTask(r)
	n.State = pb.TaskApprovalState_TASK_APPROVAL_STATE_WAITING
	n.BudgetLimits = n.BudgetPolicy
	n.Grants = nil
	n.ClaimedBy = ""
	n.ClaimExpiresAt = nil
	n.DecidedBy = ""
	n.DecidedAt = nil
	for _, m := range n.Continuation.Messages {
		if m.PreparedToolCall != nil {
			m.PreparedToolCall.Approvals = nil
		}
	}
	if e = s.write(ctx, r, n); e != nil {
		return nil, e
	}
	return &pb.RecoverTaskApprovalResponse{Record: taskMetadata(n)}, nil
}
func (s *TaskApprovalStore) CheckGrantTaskApproval(_ context.Context, q *pb.CheckGrantTaskApprovalRequest) (*pb.CheckGrantTaskApprovalResponse, error) {
	if q == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	r, e := s.load(q.Id, q.Owner)
	if e != nil {
		return nil, e
	}
	if e = checkTaskExecution(r, q.Actor, q.ClaimToken); e != nil {
		return nil, e
	}
	out := &pb.CheckGrantTaskApprovalResponse{}
	for _, g := range r.Grants {
		if g.Action == q.Action && g.Resource == q.Resource {
			out.Granted = true
			break
		}
	}
	return out, nil
}
