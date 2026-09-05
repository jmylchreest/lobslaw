package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
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

	// MinFireInterval is the floor between two attempts to fire.
	//
	// computeSleepDuration returns zero for a past-due task so the loop
	// fires it at once. That is right when the attempt succeeds and a
	// busy-wait when it cannot: fireDue declines to fire before its
	// post-election barrier completes, when this node is not the
	// leader, and when another node holds the claim — and in each of
	// those the task stays past-due, so the next pass computes zero
	// again, immediately, for as long as the condition lasts. Each of
	// those passes reads both buckets.
	//
	// The first attempt at a due task is not delayed; only a repeat
	// inside this window is. Zero picks 250ms.
	MinFireInterval time.Duration

	// WakeDebounce is how long the loop waits after a store change
	// before recomputing what is due next, so that a burst of writes
	// costs one recomputation rather than one per write.
	//
	// The recomputation reads both buckets, and the wake fires on every
	// applied entry touching either — so without this, write volume and
	// scan volume are the same number. Zero picks 50ms, which is far
	// below the resolution anything scheduled here cares about and far
	// above the cost of a burst.
	WakeDebounce time.Duration

	// Logger is used for structured log output. Nil → slog.Default().
	Logger *slog.Logger
}

// Raft is the subset of *memory.RaftNode the scheduler needs. Kept as
// an interface so tests can substitute a fake without spinning up a
// real consensus group.
type Raft interface {
	Apply(data []byte, timeout time.Duration) (any, error)
	FSM() *memory.FSM
	// IsLeader reports whether this node holds raft leadership.
	// Scheduler firing is a leader-only workload — followers don't
	// scan, don't claim, don't fire.
	IsLeader() bool
	// Barrier appends a no-op log entry and blocks until applied.
	// Used after each leadership transition to ensure the FSM has
	// fully caught up before fireDue scans — without this guarantee,
	// the scheduler reads mid-replay state (e.g. a CLAIM that put
	// the record in pending+claimed-by-X but before the matching
	// mark-done) and re-fires already-completed commitments. That
	// race produced the "commitment fires on every restart" bug.
	Barrier(timeout time.Duration) error
}

// Scheduler owns the sleep-until-due loop, the HandlerRegistry, and
// the wake channel. Constructed once per node; started with Run.
type Scheduler struct {
	cfg        Config
	raft       Raft
	handlers   *HandlerRegistry
	log        *slog.Logger
	cronParser cron.Parser

	wakeCh chan struct{}

	// started flips to true after Run starts to protect against a
	// second concurrent Run on the same Scheduler — benign but
	// wasteful.
	startedMu sync.Mutex
	started   bool

	// barrierMu guards barrierDone — flipped true after the first
	// successful Barrier() following each leadership transition.
	// Reset to false when leadership is lost or we observe we're
	// no longer the leader. Used to gate fireDue: don't fire until
	// the post-election barrier has been applied (which guarantees
	// FSM has caught up to the leader's view).
	barrierMu   sync.Mutex
	barrierDone bool

	// dueMu guards the cached next-due instant.
	//
	// computeSleepDuration used to answer "how long until something is
	// due" by scanning both buckets and proto-unmarshalling every
	// record in them, once per pass round the loop. An allocation
	// profile of a node with four scheduled tasks found 99% of
	// everything it had ever allocated on that path — 243GB over four
	// hours — because the loop goes round once per store change, not
	// once per due time.
	//
	// The answer is an absolute instant, so it stays true as time
	// passes and only a change to the buckets can falsify it. Cached
	// until something says otherwise.
	dueMu    sync.Mutex
	dueAt    time.Time
	dueValid bool

	// fireAttempts counts calls into fireDue, and lastDecline records
	// why the most recent one did not fire.
	//
	// A scheduler that cannot fire is silent. fireDue returns early on
	// a follower and before its post-election barrier completes, and
	// both are normal enough that neither logs — so "no tasks are
	// running" and "no tasks are due" look identical from outside, and
	// the difference only shows up as work that never happened.
	fireAttempts atomic.Int64
	lastDecline  atomic.Value // string

	// dueScans counts how often the buckets have actually been read to
	// answer "what is due next".
	//
	// Here because the failure this file guards against is invisible
	// otherwise: a loop that spins produces correct behaviour, no
	// errors and no log lines, and announces itself only as an
	// allocation profile nobody was looking at. A number a test can
	// assert on is the difference between that being caught and being
	// discovered four hours later.
	dueScans atomic.Int64
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
	if cfg.WakeDebounce <= 0 {
		cfg.WakeDebounce = 50 * time.Millisecond
	}
	if cfg.MinFireInterval <= 0 {
		cfg.MinFireInterval = 250 * time.Millisecond
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
	// Descriptor support (@hourly, @daily, @weekly, @monthly, @yearly,
	// @midnight, @every) lets operators write less-cryptic schedules.
	// Without it the parser rejects "@hourly" — which broke the
	// auto-seeded session-prune task. Standard 5-field minute|hour|
	// dom|month|dow expressions still work.
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

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

	// A scheduler that never fires should say so.
	//
	// fireDue declines silently on a follower and before the barrier,
	// because both are ordinary. What is not ordinary is staying in
	// either state: the loop keeps running, the logs stay clean, and
	// nothing scheduled happens — which is indistinguishable from
	// having nothing to do. This says the difference out loud, once,
	// after long enough that it cannot be the ordinary case.
	stallAfter := 3 * s.cfg.MaxSleep
	started := time.Now()
	stallReported := false

	var lastFire time.Time
	for {
		if !stallReported && time.Since(started) > stallAfter {
			s.barrierMu.Lock()
			done := s.barrierDone
			s.barrierMu.Unlock()
			if !done {
				why, _ := s.lastDecline.Load().(string)
				if why == "" {
					why = "fireDue has not been reached"
				}
				s.log.Warn("scheduler: nothing has been able to fire since startup; scheduled tasks are not running",
					"for", time.Since(started).Round(time.Second),
					"reason", why,
					"fire_attempts", s.fireAttempts.Load(),
					"is_leader", s.raft.IsLeader(),
				)
				stallReported = true
			} else {
				stallReported = true
			}
		}
		now := time.Now()
		wait := s.computeSleepDuration(now)
		// Never spin. A zero wait means something is past due; if the
		// last attempt to fire it was moments ago it did not take, and
		// hammering the store until it does is how a scheduler with
		// four tasks allocates gigabytes.
		if wait <= 0 && !lastFire.IsZero() {
			if since := now.Sub(lastFire); since < s.cfg.MinFireInterval {
				wait = s.cfg.MinFireInterval - since
			}
		}
		timer := time.NewTimer(wait)

		select {
		case <-timer.C:
			// Fire anything due as of now. Whatever fires moves its own
			// next-due, so the cached answer is stale by definition.
			lastFire = time.Now()
			s.fireDue(ctx, lastFire)
			s.invalidateNextDue()
		case <-s.wakeCh:
			timer.Stop()
			s.invalidateNextDue()
			// A burst of applies is one change as far as this loop is
			// concerned. Without the pause, the recomputation below runs
			// once per applied entry, and it reads both buckets.
			if !s.settleAfterWake(ctx) {
				return nil
			}
		case <-ctx.Done():
			timer.Stop()
			s.log.Info("scheduler: stopping", "err", ctx.Err())
			return nil
		}
	}
}

// settleAfterWake waits out a burst of store changes, returning false
// if the context ended while it waited.
//
// The wake channel holds one pending signal, so a burst already
// collapses to a single delivery — but the loop is fast enough to come
// back round and collect the next one, and the next, each time paying
// for a full scan of both buckets. The pause bounds that to one scan
// per debounce window however hard the store is being written.
func (s *Scheduler) settleAfterWake(ctx context.Context) bool {
	t := time.NewTimer(s.cfg.WakeDebounce)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
		return false
	}
	// Anything that arrived during the pause is covered by the
	// recomputation about to happen, so drop it rather than going round
	// again for it.
	select {
	case <-s.wakeCh:
	default:
	}
	return true
}

// invalidateNextDue drops the cached instant.
func (s *Scheduler) invalidateNextDue() {
	s.dueMu.Lock()
	s.dueValid = false
	s.dueMu.Unlock()
}

// cachedNextDue returns the next due instant, scanning only when the
// cached answer has been invalidated.
//
// An absolute time rather than a duration, so it survives the passage
// of time: the loop wakes every MaxSleep whether or not anything is
// due, and re-deriving the same instant on each of those wakes is the
// bulk of what this cache removes.
func (s *Scheduler) cachedNextDue(now time.Time) (time.Time, error) {
	s.dueMu.Lock()
	if s.dueValid {
		at := s.dueAt
		s.dueMu.Unlock()
		return at, nil
	}
	s.dueMu.Unlock()

	at, err := s.nextDueTime(now)
	if err != nil {
		return time.Time{}, err
	}

	s.dueMu.Lock()
	// Only recorded if nothing invalidated the cache while the scan was
	// running. A change that landed mid-scan may not be reflected in
	// what it returned, and caching it would hide that change until the
	// next one.
	if !s.dueValid {
		s.dueAt = at
		s.dueValid = true
	}
	s.dueMu.Unlock()
	return at, nil
}

// ensureBarrierDone calls raft.Barrier once per leadership term to
// guarantee the FSM has caught up to this leader's view. Returns
// true once the barrier has succeeded (now safe to scan + fire).
// Returns false on barrier failure (timeout, leadership loss) — we
// retry on the next tick. The Barrier blocks for up to 5s; that's
// our tolerance for boot-time replay completion.
func (s *Scheduler) ensureBarrierDone() bool {
	s.barrierMu.Lock()
	if s.barrierDone {
		s.barrierMu.Unlock()
		return true
	}
	s.barrierMu.Unlock()

	if err := s.raft.Barrier(5 * time.Second); err != nil {
		s.log.Warn("scheduler: barrier failed; deferring fire to next tick",
			"err", err)
		return false
	}

	s.barrierMu.Lock()
	s.barrierDone = true
	s.barrierMu.Unlock()
	s.log.Info("scheduler: barrier complete; FSM caught up — fire enabled")
	return true
}

func (s *Scheduler) resetBarrierDone() {
	s.barrierMu.Lock()
	s.barrierDone = false
	s.barrierMu.Unlock()
}

// computeSleepDuration returns how long to sleep until either the
// next due time or MaxSleep, whichever is sooner. Past-due tasks
// return zero so the loop immediately fires.
//
// Followers always sleep MaxSleep — they have nothing to do (fireDue
// is a no-op for them) and shouldn't burn CPU spinning on past-due
// records they can't claim. They wake on leadership transition via
// wakeCh.
func (s *Scheduler) computeSleepDuration(now time.Time) time.Duration {
	if !s.raft.IsLeader() {
		return s.cfg.MaxSleep
	}
	next, err := s.cachedNextDue(now)
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
	s.dueScans.Add(1)
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
// next-after-LastRun, falling back to CreatedAt for never-fired
// tasks.
//
// Anchoring at CreatedAt (not now) is load-bearing: cron.Next(t)
// returns the next firing STRICTLY AFTER t, so anchoring at now
// skips today's firing once it's passed and silently shifts every
// missed task to tomorrow. A task created at 04:44 with schedule
// "0 7 * * *" must fire at 07:00 the same day — anchoring at the
// creation time gets us that, even when we re-evaluate at 07:11.
func (s *Scheduler) taskNextRun(t *lobslawv1.ScheduledTaskRecord, now time.Time) (time.Time, error) {
	if t.NextRun != nil && !t.NextRun.AsTime().IsZero() {
		return t.NextRun.AsTime(), nil
	}
	schedule, err := s.cronParser.Parse(t.Schedule)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse schedule %q: %w", t.Schedule, err)
	}
	var anchor time.Time
	switch {
	case t.LastRun != nil && !t.LastRun.AsTime().IsZero():
		anchor = t.LastRun.AsTime()
	case t.CreatedAt != nil && !t.CreatedAt.AsTime().IsZero():
		anchor = t.CreatedAt.AsTime()
	default:
		anchor = now
	}
	return schedule.Next(anchor), nil
}

// fireDue claims + dispatches everything due at now. Claim failures
// mean someone else already won — skip silently. Dispatch runs in a
// goroutine so one slow handler doesn't block the tick.
//
// Leader-only: followers return immediately. Without this gate the
// follower's loop spins on past-due records (computeSleepDuration
// returns 0 because they're still pending in the local FSM view;
// fireDue calls applyClaim which gets ErrNotLeader; tight loop
// burns CPU + log lines).
//
// FSM-caught-up gate: also returns if the FSM hasn't applied every
// committed log entry yet. hashicorp/raft can elect this node
// leader the moment its log is up to date, but FSM.Apply runs
// asynchronously after that — so a scheduler fire that happens
// during the "FSM still catching up" window reads intermediate
// replay state (e.g. a CLAIM that put the record in
// pending+claimed-by-X but the matching mark-done isn't applied
// yet) and re-fires what's actually a completed commitment. That
// race produced the user-observed "commitment fires on every
// restart" bug.
func (s *Scheduler) fireDue(ctx context.Context, now time.Time) {
	s.fireAttempts.Add(1)
	if !s.raft.IsLeader() {
		s.lastDecline.Store("not the leader")
		s.resetBarrierDone()
		return
	}
	if !s.ensureBarrierDone() {
		s.lastDecline.Store("post-election barrier has not completed")
		return
	}
	s.lastDecline.Store("")
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
		due := time.Time{}
		if c.DueAt != nil {
			due = c.DueAt.AsTime()
		}
		s.log.Debug("scheduler: scan commitment",
			"id", c.Id, "status", c.Status, "due_at", due,
			"claimed_by", c.ClaimedBy, "now", now)
		if c.Status != string(statusPending) {
			continue
		}
		if extractClaimer(c.ClaimedBy, c.ClaimExpiresAt, now) != "" {
			continue
		}
		if c.DueAt == nil || c.DueAt.AsTime().After(now) {
			continue
		}
		s.log.Info("scheduler: firing commitment",
			"id", c.Id, "handler_ref", c.HandlerRef,
			"due_at", due, "lateness", now.Sub(due))
		s.tryFireCommitment(ctx, c, now)
	}
}

// tryFireTask attempts the CAS claim, fires the handler, then writes
// back the updated record (NextRun advanced, LastRun set, claim
// cleared for the next tick). Any step failing aborts the firing;
// future ticks retry.
//
// ExpectedClaimer is the EXACT stored ClaimedBy value (from the
// scanned record), not the expiry-aware "" reinterpretation. The
// FSM does deterministic CAS — expiry handling lives here at the
// scan layer (extractClaimer above filters out expired holders so
// fireDue chooses to attempt the claim) but the on-the-wire CAS
// must match the actual stored value or the entry won't replay.
func (s *Scheduler) tryFireTask(ctx context.Context, t *lobslawv1.ScheduledTaskRecord, now time.Time) {
	updated := proto.Clone(t).(*lobslawv1.ScheduledTaskRecord)
	updated.ClaimedBy = s.cfg.NodeID
	updated.ClaimExpiresAt = timestamppb.New(now.Add(s.cfg.ClaimTTL))

	if err := s.applyClaim(&lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: t.Id,
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: updated,
		},
		ExpectedClaimer:  t.ClaimedBy,
		ExpectedRevision: &t.Revision,
	}); err != nil {
		if errors.Is(err, memory.ErrClaimConflict) {
			s.log.Debug("scheduler: task claimed by another node", "task_id", t.Id)
			return
		}
		s.log.Warn("scheduler: task claim failed", "task_id", t.Id, "err", err)
		return
	}
	// A successful CAS wrote expected+1. Track it locally rather than
	// reading it back: the FSM's return value does not survive a
	// forwarded write, and re-reading would race with anything else
	// touching the record. The increment is an invariant of the CAS,
	// not a guess.
	updated.Revision = t.Revision + 1

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
		ExpectedClaimer:  s.cfg.NodeID,
		ExpectedRevision: &t.Revision,
	})
	if err == nil {
		return
	}
	if !errors.Is(err, memory.ErrClaimConflict) {
		s.log.Warn("scheduler: completeTask apply failed", "task_id", t.Id, "err", err)
		return
	}

	// The record moved under us. Logging and walking away would leave
	// NextRun un-advanced and the claim set until TTL, so the task
	// stalls for a full TTL and then re-fires — the failure mode this
	// whole mechanism exists to prevent. Re-read and retry onto
	// current state instead.
	//
	// Bounded, and it stops as soon as the record says the work is no
	// longer ours: an operator edit is worth merging onto, another
	// node's claim is not.
	for range completeRetries {
		current, rerr := s.loadTask(t.Id)
		if rerr != nil || current == nil {
			s.log.Warn("scheduler: completeTask re-read failed",
				"task_id", t.Id, "err", rerr)
			return
		}
		if current.ClaimedBy != s.cfg.NodeID {
			s.log.Info("scheduler: completion abandoned, claim is no longer ours",
				"task_id", t.Id, "claimed_by", current.ClaimedBy)
			return
		}
		merged := proto.Clone(current).(*lobslawv1.ScheduledTaskRecord)
		merged.LastRun = timestamppb.New(firedAt)
		if !next.IsZero() {
			merged.NextRun = timestamppb.New(next)
		}
		merged.ClaimedBy = ""
		merged.ClaimExpiresAt = nil

		err = s.applyClaim(&lobslawv1.LogEntry{
			Op:               lobslawv1.LogOp_LOG_OP_CLAIM,
			Id:               t.Id,
			Payload:          &lobslawv1.LogEntry_ScheduledTask{ScheduledTask: merged},
			ExpectedClaimer:  s.cfg.NodeID,
			ExpectedRevision: &current.Revision,
		})
		if err == nil {
			return
		}
		if !errors.Is(err, memory.ErrClaimConflict) {
			break
		}
	}
	s.log.Warn("scheduler: completeTask gave up after retries", "task_id", t.Id, "err", err)
}

// completeRetries bounds the re-read loop. Contention here is between
// this node and an operator edit, not a stampede, so a couple of
// attempts is plenty and an unbounded loop would be the worse bug.
const completeRetries = 3

// loadTask reads one scheduled task from the local store.
func (s *Scheduler) loadTask(id string) (*lobslawv1.ScheduledTaskRecord, error) {
	raw, err := s.raft.FSM().Store().Get(memory.BucketScheduledTasks, id)
	if err != nil {
		return nil, err
	}
	var rec lobslawv1.ScheduledTaskRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
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
		Op:               lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:               t.Id,
		Payload:          &lobslawv1.LogEntry_ScheduledTask{ScheduledTask: updated},
		ExpectedClaimer:  s.cfg.NodeID,
		ExpectedRevision: &t.Revision,
	})
	if err != nil {
		s.log.Warn("scheduler: release task claim failed", "task_id", t.Id, "err", err)
	}
}

// tryFireCommitment mirrors tryFireTask for one-shot commitments.
// On success the handler runs + commitment is marked Done.
//
// ExpectedClaimer is c.ClaimedBy (exact value), not the expiry-
// aware reinterpretation. See tryFireTask for the rationale —
// FSM CAS must be deterministic for replay correctness.
func (s *Scheduler) tryFireCommitment(ctx context.Context, c *lobslawv1.AgentCommitment, now time.Time) {
	updated := proto.Clone(c).(*lobslawv1.AgentCommitment)
	updated.ClaimedBy = s.cfg.NodeID
	updated.ClaimExpiresAt = timestamppb.New(now.Add(s.cfg.ClaimTTL))

	if err := s.applyClaim(&lobslawv1.LogEntry{
		Op:               lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:               c.Id,
		Payload:          &lobslawv1.LogEntry_Commitment{Commitment: updated},
		ExpectedClaimer:  c.ClaimedBy,
		ExpectedRevision: &c.Revision,
	}); err != nil {
		if errors.Is(err, memory.ErrClaimConflict) {
			return
		}
		s.log.Warn("scheduler: commitment claim failed", "id", c.Id, "err", err)
		return
	}
	updated.Revision = c.Revision + 1

	go s.runCommitmentHandler(ctx, updated)
}

// runCommitmentHandler marks the commitment Done in raft FIRST,
// then runs the handler. This is at-most-once delivery: if the
// handler errors, the user doesn't get the message — but we never
// re-fire on a leader restart that happens between dispatch and
// completion-apply (which was the bug before this ordering flip).
// Re-running is the much worse failure mode for one-shots: a
// "remind me at 9am" that fires twice is a worse user experience
// than one that silently loses a single delivery on a transport
// hiccup, which is rare and visible in the handler-error log.
func (s *Scheduler) runCommitmentHandler(ctx context.Context, c *lobslawv1.AgentCommitment) {
	handler, ok := s.handlers.GetCommitmentHandler(c.HandlerRef)
	if !ok {
		s.log.Error("scheduler: no commitment handler", "id", c.Id, "handler_ref", c.HandlerRef)
		s.releaseCommitmentClaim(ctx, c)
		return
	}

	// A handler that opted into Idempotent() runs BEFORE completion.
	// The default ordering below protects a human from a duplicate
	// message; this one protects work that is already running and
	// already being billed from being dropped on the floor.
	if s.handlers.IsIdempotent(c.HandlerRef) {
		s.runIdempotentCommitment(ctx, handler, c)
		return
	}

	updated := proto.Clone(c).(*lobslawv1.AgentCommitment)
	updated.Status = string(statusDone)
	updated.ClaimedBy = ""
	updated.ClaimExpiresAt = nil
	if err := s.applyClaim(&lobslawv1.LogEntry{
		Op:               lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:               c.Id,
		Payload:          &lobslawv1.LogEntry_Commitment{Commitment: updated},
		ExpectedClaimer:  s.cfg.NodeID,
		ExpectedRevision: &c.Revision,
	}); err != nil {
		// Couldn't mark done — typically means the claim TTL expired
		// and another node took over. Skip handler; the new claimer
		// will run it. Avoids a double-fire if our claim was stolen.
		s.log.Warn("scheduler: complete commitment failed (skipping handler to avoid double-fire)",
			"id", c.Id, "err", err)
		return
	}

	if err := handler(ctx, c); err != nil {
		s.log.Error("scheduler: commitment handler error (delivery lost — at-most-once)",
			"id", c.Id, "handler_ref", c.HandlerRef, "err", err)
	}
}

// runIdempotentCommitment is the at-least-once ordering: run first,
// then record the outcome.
//
// Three outcomes rather than two. A polling handler is not finished
// when it returns — it is finished when the JOB is — so RetryAfter
// re-arms the commitment instead of closing it. If this node dies
// anywhere in here, the claim TTL expires and another node re-runs
// the handler against the same still-pending commitment, which is
// exactly the recovery the default ordering cannot offer.
func (s *Scheduler) runIdempotentCommitment(ctx context.Context, handler CommitmentHandler, c *lobslawv1.AgentCommitment) {
	err := handler(ctx, c)

	if r, ok := AsRetryAfter(err); ok {
		s.rearmCommitment(c, r)
		return
	}

	updated := proto.Clone(c).(*lobslawv1.AgentCommitment)
	updated.Status = string(statusDone)
	updated.ClaimedBy = ""
	updated.ClaimExpiresAt = nil
	if applyErr := s.applyClaim(&lobslawv1.LogEntry{
		Op:               lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:               c.Id,
		Payload:          &lobslawv1.LogEntry_Commitment{Commitment: updated},
		ExpectedClaimer:  s.cfg.NodeID,
		ExpectedRevision: &c.Revision,
	}); applyErr != nil {
		// The claim was stolen mid-run, so the work may be repeated by
		// the new claimer. Harmless by construction — the handler said
		// it was idempotent — but worth saying out loud.
		s.log.Warn("scheduler: complete idempotent commitment failed (may re-run)",
			"id", c.Id, "err", applyErr)
	}
	if err != nil {
		s.log.Error("scheduler: idempotent commitment handler error",
			"id", c.Id, "handler_ref", c.HandlerRef, "err", err)
	}
}

// rearmCommitment leaves the commitment pending and moves it to the
// time the handler asked for, releasing the claim so any node may
// take the next poll.
func (s *Scheduler) rearmCommitment(c *lobslawv1.AgentCommitment, r *RetryAfter) {
	updated := proto.Clone(c).(*lobslawv1.AgentCommitment)
	updated.DueAt = timestamppb.New(r.At)
	updated.ClaimedBy = ""
	updated.ClaimExpiresAt = nil
	if err := s.applyClaim(&lobslawv1.LogEntry{
		Op:               lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:               c.Id,
		Payload:          &lobslawv1.LogEntry_Commitment{Commitment: updated},
		ExpectedClaimer:  s.cfg.NodeID,
		ExpectedRevision: &c.Revision,
	}); err != nil {
		// Could not re-arm. The commitment stays pending with its old
		// DueAt and an expiring claim, so it is retried anyway — later
		// than asked, but never lost.
		s.log.Warn("scheduler: re-arm commitment failed (will retry via claim expiry)",
			"id", c.Id, "err", err)
		return
	}
	s.log.Debug("scheduler: commitment re-armed",
		"id", c.Id, "due_at", r.At, "reason", r.Reason)
}

func (s *Scheduler) releaseCommitmentClaim(_ context.Context, c *lobslawv1.AgentCommitment) {
	updated := proto.Clone(c).(*lobslawv1.AgentCommitment)
	updated.ClaimedBy = ""
	updated.ClaimExpiresAt = nil
	err := s.applyClaim(&lobslawv1.LogEntry{
		Op:               lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:               c.Id,
		Payload:          &lobslawv1.LogEntry_Commitment{Commitment: updated},
		ExpectedClaimer:  s.cfg.NodeID,
		ExpectedRevision: &c.Revision,
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
