package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// ErrNoHandler fires when a due task references a HandlerRef that
// no one registered. The scheduler logs + CAS-releases the claim
// so another node can try (though it'll hit the same error unless
// handlers differ across nodes — which they shouldn't).
var ErrNoHandler = errors.New("scheduler: no handler registered for ref")

// Config tunes the scheduler. All fields optional — NewScheduler
// picks sensible defaults for zero values.
type Config struct {
	// NodeID stamps claims so tests + audit can tell who ran what.
	// Required — an unset NodeID means "I can't claim anything" and
	// NewScheduler returns an error.
	NodeID string

	// ClaimTTL is how long a claim is valid before it's treated as
	// abandoned by the FSM's extractClaimer. Gives a crashed node
	// time to recover while still ensuring forward progress. Zero
	// picks 5 minutes.
	ClaimTTL time.Duration

	// MaxSleep caps how long the sleep-until-due loop ever waits
	// before recomputing. Belt + braces: if a wake signal is lost
	// (callback panic, etc.) the scheduler self-heals within this
	// window. Zero picks 60 seconds.
	MaxSleep time.Duration

	// RaftApplyTimeout is how long a claim proposal waits for Raft
	// consensus. Zero picks 5 seconds.
	RaftApplyTimeout time.Duration

	// Logger is used for structured log output. Nil → slog.Default().
	Logger *slog.Logger
}

// Raft is the subset of *memory.RaftNode the scheduler needs. Kept as
// an interface so tests can substitute a fake without spinning up a
// real consensus group.
type Raft interface {
	Apply(data []byte, timeout time.Duration) (any, error)
	FSM() *memory.FSM
}

// Scheduler owns the sleep-until-due loop, the HandlerRegistry, and
// the wake channel. Constructed once per node; started with Run.
type Scheduler struct {
	cfg       Config
	raft      Raft
	handlers  *HandlerRegistry
	log       *slog.Logger
	cronParser cron.Parser

	wakeCh chan struct{}

	// started flips to true after Run starts to protect against a
	// second concurrent Run on the same Scheduler — benign but
	// wasteful.
	startedMu sync.Mutex
	started   bool
}

// NewScheduler constructs a scheduler. Fails when required config
// is missing so a misconfigured node crashes at boot rather than
// silently refusing to fire tasks.
func NewScheduler(cfg Config, raft Raft, handlers *HandlerRegistry) (*Scheduler, error) {
	if cfg.NodeID == "" {
		return nil, errors.New("scheduler: NodeID is required")
	}
	if raft == nil {
		return nil, errors.New("scheduler: Raft is required")
	}
	if handlers == nil {
		handlers = NewHandlerRegistry()
	}
	if cfg.ClaimTTL <= 0 {
		cfg.ClaimTTL = 5 * time.Minute
	}
	if cfg.MaxSleep <= 0 {
		cfg.MaxSleep = 60 * time.Second
	}
	if cfg.RaftApplyTimeout <= 0 {
		cfg.RaftApplyTimeout = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	// Standard cron syntax (minute-resolution, 5 fields). If an
	// operator writes a 6-field expression with a seconds position
	// it'll fail to parse — document and keep strict rather than
	// guessing.
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

	s := &Scheduler{
		cfg:        cfg,
		raft:       raft,
		handlers:   handlers,
		log:        cfg.Logger,
		cronParser: parser,
		// Buffer of 1 so repeated Notify calls coalesce — the
		// scheduler only needs to know "something changed," not how
		// many changes happened.
		wakeCh: make(chan struct{}, 1),
	}

	// Wire the FSM callback so writes originating on any node
	// (including this one) wake the loop.
	raft.FSM().SetSchedulerChangeCallback(s.Notify)
	return s, nil
}

// Notify wakes the scheduler out of its sleep-until-due window so it
// re-scans. Non-blocking: if a prior wake is already queued, this
// call is a no-op (coalesced).
func (s *Scheduler) Notify() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

// Handlers returns the registry so callers can register + list.
func (s *Scheduler) Handlers() *HandlerRegistry { return s.handlers }

// Run is the main loop. Blocks until ctx is cancelled. Safe to call
// once per Scheduler; a second call returns immediately.
func (s *Scheduler) Run(ctx context.Context) error {
	s.startedMu.Lock()
	if s.started {
		s.startedMu.Unlock()
		return nil
	}
	s.started = true
	s.startedMu.Unlock()

	s.log.Info("scheduler: starting",
		"node_id", s.cfg.NodeID,
		"max_sleep", s.cfg.MaxSleep,
		"claim_ttl", s.cfg.ClaimTTL,
	)

	for {
		wait := s.computeSleepDuration(time.Now())
		timer := time.NewTimer(wait)

		select {
		case <-timer.C:
			// Fire anything due as of now.
			s.fireDue(ctx, time.Now())
		case <-s.wakeCh:
			timer.Stop()
			// Drop back to the top — recompute next-due from scratch.
		case <-ctx.Done():
			timer.Stop()
			s.log.Info("scheduler: stopping", "err", ctx.Err())
			return nil
		}
	}
}

// computeSleepDuration returns how long to sleep until either the
// next due time or MaxSleep, whichever is sooner. Past-due tasks
// return zero so the loop immediately fires.
func (s *Scheduler) computeSleepDuration(now time.Time) time.Duration {
	next, err := s.nextDueTime(now)
	if err != nil {
		s.log.Warn("scheduler: compute next-due failed — using MaxSleep", "err", err)
		return s.cfg.MaxSleep
	}
	if next.IsZero() {
		return s.cfg.MaxSleep
	}
	d := time.Until(next)
	if d < 0 {
		return 0
	}
	if d > s.cfg.MaxSleep {
		return s.cfg.MaxSleep
	}
	return d
}

// nextDueTime walks all scheduled tasks + pending commitments and
// returns the earliest firing time in the future (or in the past, if
// something is already overdue). Returns zero time + nil error when
// there's nothing scheduled.
func (s *Scheduler) nextDueTime(now time.Time) (time.Time, error) {
	var earliest time.Time
	pick := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}

	tasks, err := s.listScheduledTasks()
	if err != nil {
		return time.Time{}, err
	}
	for _, t := range tasks {
		if !t.Enabled {
			continue
		}
		due, err := s.taskNextRun(t, now)
		if err != nil {
			s.log.Warn("scheduler: task has unparseable schedule — skipping",
				"task_id", t.Id, "schedule", t.Schedule, "err", err)
			continue
		}
		pick(due)
	}

	commits, err := s.listCommitments()
	if err != nil {
		return time.Time{}, err
	}
	for _, c := range commits {
		if c.Status != string(statusPending) {
			continue
		}
		if c.DueAt != nil {
			pick(c.DueAt.AsTime())
		}
	}

	return earliest, nil
}

// taskNextRun returns a scheduled task's next firing time given the
// cluster's current clock. If NextRun is already set and in the
// future, honour it; otherwise recompute from the cron schedule's
// next-after-LastRun (or next-after-now if LastRun is zero).
func (s *Scheduler) taskNextRun(t *lobslawv1.ScheduledTaskRecord, now time.Time) (time.Time, error) {
	if t.NextRun != nil && !t.NextRun.AsTime().IsZero() {
		return t.NextRun.AsTime(), nil
	}
	schedule, err := s.cronParser.Parse(t.Schedule)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse schedule %q: %w", t.Schedule, err)
	}
	anchor := now
	if t.LastRun != nil && !t.LastRun.AsTime().IsZero() {
		anchor = t.LastRun.AsTime()
	}
	return schedule.Next(anchor), nil
}

// fireDue claims + dispatches everything due at now. Claim failures
// mean someone else already won — skip silently. Dispatch runs in a
// goroutine so one slow handler doesn't block the tick.
func (s *Scheduler) fireDue(ctx context.Context, now time.Time) {
	tasks, err := s.listScheduledTasks()
	if err != nil {
		s.log.Error("scheduler: list tasks failed", "err", err)
		return
	}
	for _, t := range tasks {
		if !t.Enabled {
			continue
		}
		// Skip anything currently claimed — either another node is
		// working on it OR this same scheduler's previous fire is
		// still in flight. Without this skip, the loop's own
		// async-handler write latency races with the next iteration
		// and fires the task twice against its own claim.
		if extractClaimer(t.ClaimedBy, t.ClaimExpiresAt, now) != "" {
			continue
		}
		due, err := s.taskNextRun(t, now)
		if err != nil || due.IsZero() || due.After(now) {
			continue
		}
		s.tryFireTask(ctx, t, now)
	}

	commits, err := s.listCommitments()
	if err != nil {
		s.log.Error("scheduler: list commitments failed", "err", err)
		return
	}
	for _, c := range commits {
		if c.Status != string(statusPending) {
			continue
		}
		if extractClaimer(c.ClaimedBy, c.ClaimExpiresAt, now) != "" {
			continue
		}
		if c.DueAt == nil || c.DueAt.AsTime().After(now) {
			continue
		}
		s.tryFireCommitment(ctx, c, now)
	}
}

// tryFireTask attempts the CAS claim, fires the handler, then writes
// back the updated record (NextRun advanced, LastRun set, claim
// cleared for the next tick). Any step failing aborts the firing;
// future ticks retry.
func (s *Scheduler) tryFireTask(ctx context.Context, t *lobslawv1.ScheduledTaskRecord, now time.Time) {
	prev := extractClaimer(t.ClaimedBy, t.ClaimExpiresAt, now)
	updated := proto.Clone(t).(*lobslawv1.ScheduledTaskRecord)
	updated.ClaimedBy = s.cfg.NodeID
	updated.ClaimExpiresAt = timestamppb.New(now.Add(s.cfg.ClaimTTL))

	if err := s.applyClaim(&lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: t.Id,
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: updated,
		},
		ExpectedClaimer: prev,
	}); err != nil {
		if errors.Is(err, memory.ErrClaimConflict) {
			s.log.Debug("scheduler: task claimed by another node", "task_id", t.Id)
			return
		}
		s.log.Warn("scheduler: task claim failed", "task_id", t.Id, "err", err)
		return
	}

	go s.runTaskHandler(ctx, updated, now)
}

// runTaskHandler dispatches through the registry, then writes back a
// record that advances NextRun + records LastRun and clears the
// claim. A handler error is logged; the next tick retries via the
// regular schedule.
func (s *Scheduler) runTaskHandler(ctx context.Context, t *lobslawv1.ScheduledTaskRecord, firedAt time.Time) {
	handler, ok := s.handlers.GetTaskHandler(t.HandlerRef)
	if !ok {
		s.log.Error("scheduler: no handler", "task_id", t.Id, "handler_ref", t.HandlerRef)
		s.releaseTaskClaim(ctx, t, firedAt)
		return
	}
	if err := handler(ctx, t); err != nil {
		s.log.Error("scheduler: task handler error",
			"task_id", t.Id, "handler_ref", t.HandlerRef, "err", err)
	}
	s.completeTask(ctx, t, firedAt)
}

// completeTask writes the post-fire state: LastRun=firedAt,
// NextRun=cron.Next(firedAt), claim cleared. Runs under a CAS so
// a retry triggered by a remote Notify doesn't silently stomp a
// subsequent re-fire.
func (s *Scheduler) completeTask(ctx context.Context, t *lobslawv1.ScheduledTaskRecord, firedAt time.Time) {
	next, err := s.computeNextCron(t.Schedule, firedAt)
	if err != nil {
		s.log.Warn("scheduler: next-cron compute failed — leaving NextRun empty",
			"task_id", t.Id, "err", err)
	}
	updated := proto.Clone(t).(*lobslawv1.ScheduledTaskRecord)
	updated.LastRun = timestamppb.New(firedAt)
	if !next.IsZero() {
		updated.NextRun = timestamppb.New(next)
	}
	updated.ClaimedBy = ""
	updated.ClaimExpiresAt = nil

	err = s.applyClaim(&lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: t.Id,
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: updated,
		},
		ExpectedClaimer: s.cfg.NodeID,
	})
	if err != nil {
		s.log.Warn("scheduler: completeTask apply failed", "task_id", t.Id, "err", err)
	}
}

// releaseTaskClaim writes the claim back to unclaimed without
// touching LastRun/NextRun. Used when the handler was missing or
// refused and we want a sibling node (with a different handler
// set) to pick the task up.
func (s *Scheduler) releaseTaskClaim(ctx context.Context, t *lobslawv1.ScheduledTaskRecord, _ time.Time) {
	updated := proto.Clone(t).(*lobslawv1.ScheduledTaskRecord)
	updated.ClaimedBy = ""
	updated.ClaimExpiresAt = nil
	err := s.applyClaim(&lobslawv1.LogEntry{
		Op:              lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:              t.Id,
		Payload:         &lobslawv1.LogEntry_ScheduledTask{ScheduledTask: updated},
		ExpectedClaimer: s.cfg.NodeID,
	})
	if err != nil {
		s.log.Warn("scheduler: release task claim failed", "task_id", t.Id, "err", err)
	}
}

// tryFireCommitment mirrors tryFireTask for one-shot commitments.
// On success the handler runs + commitment is marked Done.
func (s *Scheduler) tryFireCommitment(ctx context.Context, c *lobslawv1.AgentCommitment, now time.Time) {
	prev := extractClaimer(c.ClaimedBy, c.ClaimExpiresAt, now)
	updated := proto.Clone(c).(*lobslawv1.AgentCommitment)
	updated.ClaimedBy = s.cfg.NodeID
	updated.ClaimExpiresAt = timestamppb.New(now.Add(s.cfg.ClaimTTL))

	if err := s.applyClaim(&lobslawv1.LogEntry{
		Op:              lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:              c.Id,
		Payload:         &lobslawv1.LogEntry_Commitment{Commitment: updated},
		ExpectedClaimer: prev,
	}); err != nil {
		if errors.Is(err, memory.ErrClaimConflict) {
			return
		}
		s.log.Warn("scheduler: commitment claim failed", "id", c.Id, "err", err)
		return
	}
	go s.runCommitmentHandler(ctx, updated)
}

// runCommitmentHandler dispatches + marks the commitment Done.
func (s *Scheduler) runCommitmentHandler(ctx context.Context, c *lobslawv1.AgentCommitment) {
	handler, ok := s.handlers.GetCommitmentHandler(c.HandlerRef)
	if !ok {
		s.log.Error("scheduler: no commitment handler", "id", c.Id, "handler_ref", c.HandlerRef)
		s.releaseCommitmentClaim(ctx, c)
		return
	}
	if err := handler(ctx, c); err != nil {
		s.log.Error("scheduler: commitment handler error",
			"id", c.Id, "handler_ref", c.HandlerRef, "err", err)
	}
	updated := proto.Clone(c).(*lobslawv1.AgentCommitment)
	updated.Status = string(statusDone)
	updated.ClaimedBy = ""
	updated.ClaimExpiresAt = nil
	err := s.applyClaim(&lobslawv1.LogEntry{
		Op:              lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:              c.Id,
		Payload:         &lobslawv1.LogEntry_Commitment{Commitment: updated},
		ExpectedClaimer: s.cfg.NodeID,
	})
	if err != nil {
		s.log.Warn("scheduler: complete commitment failed", "id", c.Id, "err", err)
	}
}

func (s *Scheduler) releaseCommitmentClaim(_ context.Context, c *lobslawv1.AgentCommitment) {
	updated := proto.Clone(c).(*lobslawv1.AgentCommitment)
	updated.ClaimedBy = ""
	updated.ClaimExpiresAt = nil
	err := s.applyClaim(&lobslawv1.LogEntry{
		Op:              lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:              c.Id,
		Payload:         &lobslawv1.LogEntry_Commitment{Commitment: updated},
		ExpectedClaimer: s.cfg.NodeID,
	})
	if err != nil {
		s.log.Warn("scheduler: release commitment claim failed", "id", c.Id, "err", err)
	}
}

// applyClaim marshals the LogEntry and submits it through raft. The
// FSM's applyClaim does the atomic read-check-write; this is a thin
// wrapper that translates the response into a Go error.
func (s *Scheduler) applyClaim(entry *lobslawv1.LogEntry) error {
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal log entry: %w", err)
	}
	resp, err := s.raft.Apply(data, s.cfg.RaftApplyTimeout)
	if err != nil {
		return fmt.Errorf("raft apply: %w", err)
	}
	if ferr, ok := resp.(error); ok && ferr != nil {
		return ferr
	}
	return nil
}

// computeNextCron parses the cron expression and returns the next
// firing time strictly after anchor. Exposed as a helper so the
// task fire + complete paths share the same calculation.
func (s *Scheduler) computeNextCron(expr string, anchor time.Time) (time.Time, error) {
	if expr == "" {
		return time.Time{}, nil
	}
	schedule, err := s.cronParser.Parse(expr)
	if err != nil {
		return time.Time{}, err
	}
	return schedule.Next(anchor), nil
}

// listScheduledTasks reads every task record from the store. Returns
// the proto slice so the scheduler can walk + filter. The store
// returns a consistent snapshot per bbolt View semantics; no lock
// needed at this layer.
func (s *Scheduler) listScheduledTasks() ([]*lobslawv1.ScheduledTaskRecord, error) {
	var out []*lobslawv1.ScheduledTaskRecord
	err := s.raft.FSM().Store().ForEach(memory.BucketScheduledTasks, func(_ string, raw []byte) error {
		var r lobslawv1.ScheduledTaskRecord
		if err := proto.Unmarshal(raw, &r); err != nil {
			return err
		}
		out = append(out, &r)
		return nil
	})
	return out, err
}

func (s *Scheduler) listCommitments() ([]*lobslawv1.AgentCommitment, error) {
	var out []*lobslawv1.AgentCommitment
	err := s.raft.FSM().Store().ForEach(memory.BucketCommitments, func(_ string, raw []byte) error {
		var r lobslawv1.AgentCommitment
		if err := proto.Unmarshal(raw, &r); err != nil {
			return err
		}
		out = append(out, &r)
		return nil
	})
	return out, err
}

// statusPending / statusDone are the string forms used on the wire
// for AgentCommitment.Status. Mirrors the pkg/types constants so
// the scheduler can compare without pulling a types import here.
type commitmentStatusWire string

const (
	statusPending commitmentStatusWire = "pending"
	statusDone    commitmentStatusWire = "done"
)

// extractClaimer is the in-process mirror of FSM.extractClaimer for
// scheduler-side CAS setup. Returns what the FSM will observe when
// this node's claim proposal runs, so the scheduler can pass the
// correct ExpectedClaimer value.
func extractClaimer(claimedBy string, expiresAt *timestamppb.Timestamp, now time.Time) string {
	if claimedBy == "" {
		return ""
	}
	if expiresAt != nil && expiresAt.AsTime().Before(now) {
		return ""
	}
	return claimedBy
}
