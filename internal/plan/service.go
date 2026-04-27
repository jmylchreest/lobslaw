package plan

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// DefaultWindow is the look-ahead window used when GetPlanRequest.Window
// is unset or zero. 24h matches the "what's your plan today?" framing.
const DefaultWindow = 24 * time.Hour

// Service implements lobslawv1.PlanServiceServer. All writes go
// through Raft so they replicate to every voter; reads go through
// the local store for zero-round-trip serving.
type Service struct {
	lobslawv1.UnimplementedPlanServiceServer

	raft         raftApplier
	store        *memory.Store
	applyTimeout time.Duration
}

// raftApplier is the subset of *memory.RaftNode the service needs.
// Kept as an interface so unit tests can substitute a fake.
type raftApplier interface {
	Apply(data []byte, timeout time.Duration) (any, error)
}

// NewService wires the service to a Raft node. applyTimeout caps
// how long an AddCommitment / CancelCommitment call waits for
// consensus; zero picks 5 seconds.
func NewService(raft *memory.RaftNode, applyTimeout time.Duration) *Service {
	if applyTimeout <= 0 {
		applyTimeout = 5 * time.Second
	}
	return &Service{
		raft:         raft,
		store:        raft.FSM().Store(),
		applyTimeout: applyTimeout,
	}
}

// GetPlan aggregates commitments + scheduled tasks whose next firing
// lands within [now, now+window]. InFlightWork and CheckBackThreads
// are placeholders populated by later phases (agent in-flight tracker,
// audit threads).
func (s *Service) GetPlan(_ context.Context, req *lobslawv1.GetPlanRequest) (*lobslawv1.GetPlanResponse, error) {
	window := DefaultWindow
	if req != nil && req.Window != nil && req.Window.AsDuration() > 0 {
		window = req.Window.AsDuration()
	}

	now := time.Now()
	cutoff := now.Add(window)

	commitments, err := s.listCommitments(now, cutoff)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list commitments: %v", err)
	}
	tasks, err := s.listScheduledTasks(now, cutoff)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list scheduled tasks: %v", err)
	}

	return &lobslawv1.GetPlanResponse{
		Window:         durationpb.New(window),
		Commitments:    commitments,
		ScheduledTasks: tasks,
		// InFlight / CheckBackThreads: left nil for now; Phase 7c+
		// wires the agent's in-flight tracker + the audit bridge.
	}, nil
}

// AddCommitment persists a new AgentCommitment via Raft. Auto-fills
// id (random 32 hex), status (pending if unset), and sanitises
// out any caller-supplied claim fields — claim state is scheduler-
// internal and must only transition through LOG_OP_CLAIM.
func (s *Service) AddCommitment(_ context.Context, req *lobslawv1.AddCommitmentRequest) (*lobslawv1.AddCommitmentResponse, error) {
	if req == nil || req.Commitment == nil {
		return nil, status.Error(codes.InvalidArgument, "commitment is required")
	}
	c := proto.Clone(req.Commitment).(*lobslawv1.AgentCommitment)
	if c.Id == "" {
		id, err := randomID()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "generate id: %v", err)
		}
		c.Id = id
	}
	if c.Status == "" {
		c.Status = "pending"
	}
	if c.DueAt == nil {
		return nil, status.Error(codes.InvalidArgument, "due_at is required")
	}
	// Sanitise claim fields — they're scheduler-owned.
	c.ClaimedBy = ""
	c.ClaimExpiresAt = nil

	entry := &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      c.Id,
		Payload: &lobslawv1.LogEntry_Commitment{Commitment: c},
	}
	if err := s.apply(entry); err != nil {
		return nil, status.Errorf(codes.Internal, "apply: %v", err)
	}
	return &lobslawv1.AddCommitmentResponse{Id: c.Id}, nil
}

// CancelCommitment transitions a pending commitment to cancelled
// via CAS. Fails if the commitment is currently claimed (a handler
// is firing). Already-done / already-cancelled commitments return a
// clean error rather than silently succeeding so callers know the
// state wasn't what they expected.
func (s *Service) CancelCommitment(_ context.Context, req *lobslawv1.CancelCommitmentRequest) (*lobslawv1.CancelCommitmentResponse, error) {
	if req == nil || req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	raw, err := s.store.Get(memory.BucketCommitments, req.Id)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "commitment %q not found", req.Id)
	}
	var current lobslawv1.AgentCommitment
	if err := proto.Unmarshal(raw, &current); err != nil {
		return nil, status.Errorf(codes.Internal, "unmarshal: %v", err)
	}
	if current.Status == "cancelled" {
		return nil, status.Errorf(codes.FailedPrecondition, "commitment %q already cancelled", req.Id)
	}
	if current.Status == "done" {
		return nil, status.Errorf(codes.FailedPrecondition, "commitment %q already completed", req.Id)
	}

	// Decide ExpectedClaimer at the service layer. The FSM's CAS is
	// exact-match (deterministic across replay — see internal/memory/fsm.go
	// applyClaim doc), so it does NOT do its own time-based "expired
	// claim counts as unclaimed" reinterpretation. Plan does that here:
	//
	//   - genuinely unclaimed (ClaimedBy == "")           → expected = ""
	//   - currently claimed and not expired               → Aborted (in-flight)
	//   - claimed but expiry in the past (claimer crashed)→ expected = current.ClaimedBy
	//     so the CAS lands and the cancelled state replaces the stale claim
	expected := ""
	if current.ClaimedBy != "" {
		expiry := current.GetClaimExpiresAt()
		if expiry == nil || expiry.AsTime().After(time.Now()) {
			return nil, status.Errorf(codes.Aborted,
				"commitment %q is in-flight; retry after the current handler completes", req.Id)
		}
		// Stale claim — pass the actual ClaimedBy as expected so the
		// FSM CAS lands; the updated record clears ClaimedBy.
		expected = current.ClaimedBy
	}

	updated := proto.Clone(&current).(*lobslawv1.AgentCommitment)
	updated.Status = "cancelled"
	updated.ClaimedBy = ""
	updated.ClaimExpiresAt = nil

	entry := &lobslawv1.LogEntry{
		Op:              lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:              current.Id,
		Payload:         &lobslawv1.LogEntry_Commitment{Commitment: updated},
		ExpectedClaimer: expected,
	}
	if err := s.apply(entry); err != nil {
		if errors.Is(err, memory.ErrClaimConflict) {
			return nil, status.Errorf(codes.Aborted,
				"commitment %q is in-flight; retry after the current handler completes", req.Id)
		}
		return nil, status.Errorf(codes.Internal, "apply: %v", err)
	}
	return &lobslawv1.CancelCommitmentResponse{}, nil
}

// ---- read helpers ----

// listCommitments returns commitments whose DueAt falls in
// [now, cutoff] AND whose status is pending or in-flight. Already-
// done / cancelled commitments are filtered out — they're audit
// history, not "plan" entries.
func (s *Service) listCommitments(now, cutoff time.Time) ([]*lobslawv1.AgentCommitment, error) {
	var out []*lobslawv1.AgentCommitment
	err := s.store.ForEach(memory.BucketCommitments, func(_ string, raw []byte) error {
		var c lobslawv1.AgentCommitment
		if err := proto.Unmarshal(raw, &c); err != nil {
			return err
		}
		if c.Status != "pending" {
			return nil
		}
		if c.DueAt == nil {
			return nil
		}
		due := c.DueAt.AsTime()
		if due.Before(now) || due.After(cutoff) {
			return nil
		}
		out = append(out, &c)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].DueAt.AsTime().Before(out[j].DueAt.AsTime())
	})
	return out, nil
}

// listScheduledTasks returns enabled tasks whose NextRun (or
// cron-computed next-after-now) lies in [now, cutoff]. Disabled
// tasks are filtered even if their NextRun would otherwise match —
// a disabled task isn't "on the plan."
func (s *Service) listScheduledTasks(now, cutoff time.Time) ([]*lobslawv1.ScheduledTaskRecord, error) {
	var out []*lobslawv1.ScheduledTaskRecord
	err := s.store.ForEach(memory.BucketScheduledTasks, func(_ string, raw []byte) error {
		var t lobslawv1.ScheduledTaskRecord
		if err := proto.Unmarshal(raw, &t); err != nil {
			return err
		}
		if !t.Enabled {
			return nil
		}
		next := taskNextRun(&t, now)
		if next.IsZero() {
			return nil
		}
		if next.Before(now) || next.After(cutoff) {
			return nil
		}
		out = append(out, &t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		return taskNextRun(out[i], now).Before(taskNextRun(out[j], now))
	})
	return out, nil
}

// ---- internal helpers ----

// apply marshals the entry and submits via Raft. Translates an FSM
// error response into a Go error rather than sinking it silently.
func (s *Service) apply(entry *lobslawv1.LogEntry) error {
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal log entry: %w", err)
	}
	res, err := s.raft.Apply(data, s.applyTimeout)
	if err != nil {
		return err
	}
	if ferr, ok := res.(error); ok && ferr != nil {
		return ferr
	}
	return nil
}

// taskNextRun picks the effective "next firing time" for a task:
// the stored NextRun if set, else the current time + 1ns (so
// newly-enabled tasks still show up in the plan until the scheduler
// advances them). Cron recomputation lives in the scheduler
// package; the PlanService doesn't parse schedules — it just
// surfaces what's stored.
func taskNextRun(t *lobslawv1.ScheduledTaskRecord, now time.Time) time.Time {
	if t.NextRun != nil && !t.NextRun.AsTime().IsZero() {
		return t.NextRun.AsTime()
	}
	return now
}

// randomID produces an opaque, unguessable commitment identifier.
// 16 random bytes → 32 hex chars matches the length used elsewhere
// (prompt IDs, turn IDs) so the IDs look homogeneous across the
// system.
func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
