package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/ids"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Confirmations used to live in a per-handler Go map, which meant an
// approval could only be answered by the process that asked, and a
// restart between question and answer lost the turn. Both are visible
// to a user as "sorry, resend that".
//
// The record lives in Raft instead, and resolution is a CAS from
// PENDING using the same claim primitive the scheduler uses — so
// first writer wins CLUSTER-wide, not just within one process.

var (
	// ErrPromptNotFound is returned for an unknown or purged id.
	ErrPromptNotFound = errors.New("prompt: not found")

	// ErrPromptResolved means somebody else answered first. Expected
	// under a double-tap or two channels racing, not an error worth
	// alarming about.
	ErrPromptResolved = errors.New("prompt: already resolved")
)

// DefaultPromptTTL bounds how long a question waits for an answer.
const DefaultPromptTTL = 15 * time.Minute

// PromptStore is the Raft-backed confirmation registry.
type PromptStore struct {
	raft  raftApplier
	store *Store
	log   *slog.Logger
	ttl   time.Duration
}

type PromptStoreConfig struct {
	Raft  raftApplier
	Store *Store
	TTL   time.Duration
	Log   *slog.Logger
}

func NewPromptStore(cfg PromptStoreConfig) (*PromptStore, error) {
	if cfg.Raft == nil || cfg.Store == nil {
		return nil, errors.New("prompt store: Raft and Store are both required")
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultPromptTTL
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &PromptStore{raft: cfg.Raft, store: cfg.Store, log: log, ttl: ttl}, nil
}

func (p *PromptStore) Create(rec *lobslawv1.PromptRecord) (*lobslawv1.PromptRecord, error) {
	if rec == nil {
		return nil, errors.New("prompt: nil record")
	}
	out := proto.Clone(rec).(*lobslawv1.PromptRecord)
	if out.Id == "" {
		out.Id = ids.New()
	}
	out.Decision = lobslawv1.PromptDecision_PROMPT_DECISION_PENDING
	out.CreatedAt = timestamppb.Now()
	if out.ExpiresAt == nil {
		out.ExpiresAt = timestamppb.New(time.Now().Add(p.ttl))
	}

	if err := p.put(out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *PromptStore) Get(id string) (*lobslawv1.PromptRecord, error) {
	raw, err := p.store.Get(BucketPrompts, id)
	if err != nil {
		return nil, ErrPromptNotFound
	}
	var rec lobslawv1.PromptRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("prompt %q: decode: %w", id, err)
	}
	return &rec, nil
}

// Resolve moves a prompt out of PENDING, exactly once.
//
// The CAS is on revision rather than on a read-then-write, so two
// nodes answering simultaneously cannot both win: the FSM applies one
// and rejects the other, and the loser gets ErrPromptResolved rather
// than silently overwriting a decision the user already made.
func (p *PromptStore) Resolve(id string, decision lobslawv1.PromptDecision, scope lobslawv1.PromptScope, by string) (*lobslawv1.PromptRecord, error) {
	if decision == lobslawv1.PromptDecision_PROMPT_DECISION_PENDING ||
		decision == lobslawv1.PromptDecision_PROMPT_DECISION_UNSPECIFIED {
		return nil, fmt.Errorf("prompt: %s is not a resolution", decision)
	}
	current, err := p.Get(id)
	if err != nil {
		return nil, err
	}
	if current.Decision != lobslawv1.PromptDecision_PROMPT_DECISION_PENDING {
		return nil, ErrPromptResolved
	}

	updated := proto.Clone(current).(*lobslawv1.PromptRecord)
	updated.Decision = decision
	updated.Scope = scope
	updated.ResolvedBy = by
	// claimed_by is what the FSM compares. Setting it to the resolver
	// while expecting the empty string is the whole CAS: a second
	// resolver reads a non-empty value and its apply is refused.
	updated.ClaimedBy = by

	entry := &lobslawv1.LogEntry{
		Op:               lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:               id,
		Payload:          &lobslawv1.LogEntry_Prompt{Prompt: updated},
		ExpectedClaimer:  "",
		ExpectedRevision: &current.Revision,
	}
	if err := p.apply(entry); err != nil {
		// Only a refused CAS means somebody answered first. Collapsing
		// every failure into that would tell a user "someone else
		// already decided" when the truth was a raft timeout.
		if errors.Is(err, ErrClaimConflict) {
			return nil, ErrPromptResolved
		}
		return nil, err
	}
	return updated, nil
}

// Sweep times out every prompt past its expiry and returns how many
// it closed.
//
// A sweeper rather than time.AfterFunc, because a timer is
// per-process: a prompt created on a node that then dies would never
// time out, and the turn would sit pending forever. Leader-gated by
// the caller, so one node does this rather than all of them.
func (p *PromptStore) Sweep(now time.Time) (int, error) {
	var expired []*lobslawv1.PromptRecord
	err := p.store.ForEach(BucketPrompts, func(_ string, raw []byte) error {
		var rec lobslawv1.PromptRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return nil //nolint:nilerr // a corrupt record is not a reason to stop sweeping
		}
		if rec.Decision != lobslawv1.PromptDecision_PROMPT_DECISION_PENDING {
			return nil
		}
		if rec.ExpiresAt != nil && now.After(rec.ExpiresAt.AsTime()) {
			expired = append(expired, &rec)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("prompt sweep: %w", err)
	}

	var closed int
	for _, rec := range expired {
		if err := p.timeOut(rec, "sweeper"); err != nil {
			continue
		}
		closed++
	}
	if closed > 0 {
		p.log.Info("prompt sweep: timed out expired confirmations", "count", closed)
	}
	return closed, nil
}

// timeOut closes one expired prompt, under the same CAS a user
// answering would use. A prompt answered by a person between the
// caller's read and this write loses the race here, which is the
// correct outcome: their answer counts, not the clock's.
func (p *PromptStore) timeOut(rec *lobslawv1.PromptRecord, by string) error {
	updated := proto.Clone(rec).(*lobslawv1.PromptRecord)
	updated.Decision = lobslawv1.PromptDecision_PROMPT_DECISION_TIMED_OUT
	updated.ClaimedBy = by
	return p.apply(&lobslawv1.LogEntry{
		Op:               lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:               rec.Id,
		Payload:          &lobslawv1.LogEntry_Prompt{Prompt: updated},
		ExpectedClaimer:  "",
		ExpectedRevision: &rec.Revision,
	})
}

// DefaultSweepInterval is how often the leader looks for expired
// confirmations. A confirmation can therefore linger up to one
// interval past its expiry, which is why it is well under the
// shortest TTL a channel configures.
const DefaultSweepInterval = 30 * time.Second

// SweepLoop runs Sweep until the context ends. Intended to be driven
// by singleton.Run so exactly one node in the cluster is doing it —
// every node sweeping would be correct (the CAS refuses the
// duplicates) but would spend a raft round-trip per node per expiry.
func (p *PromptStore) SweepLoop(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-t.C:
			if _, err := p.Sweep(now); err != nil {
				// A failed sweep is not fatal: the next tick retries,
				// and the records it missed are still expired.
				p.log.Warn("prompt sweep failed", "err", err)
			}
		}
	}
}

// Wait blocks until the prompt leaves PENDING, the context ends, or
// the record expires.
//
// Polling rather than a channel: the resolution can arrive on another
// node, so there is no in-process event to wait on. The interval is a
// user-facing latency floor on the "approved, carrying on" reply, so
// it is short.
func (p *PromptStore) Wait(ctx context.Context, id string, poll time.Duration) (*lobslawv1.PromptRecord, error) {
	if poll <= 0 {
		poll = DefaultPromptPollInterval
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		rec, err := p.Get(id)
		if err != nil {
			return nil, err
		}
		if rec.Decision != lobslawv1.PromptDecision_PROMPT_DECISION_PENDING {
			return rec, nil
		}
		// A waiter closes its own expired prompt rather than waiting
		// for the leader's next sweep tick. The sweep is the backstop
		// for prompts nobody is waiting on; making a blocked caller
		// depend on it would leave a long-poll hanging for up to a
		// sweep interval after the answer window had already shut,
		// and for longer than that during an election.
		if rec.ExpiresAt != nil && time.Now().After(rec.ExpiresAt.AsTime()) {
			if err := p.timeOut(rec, "waiter"); err != nil && !errors.Is(err, ErrClaimConflict) {
				return nil, err
			}
			// Loop rather than return: on a conflict somebody resolved
			// it first, and the next read reports what they decided.
			continue
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

func (p *PromptStore) put(rec *lobslawv1.PromptRecord) error {
	return p.apply(&lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      rec.Id,
		Payload: &lobslawv1.LogEntry_Prompt{Prompt: rec},
	})
}

func (p *PromptStore) apply(entry *lobslawv1.LogEntry) error {
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("prompt: marshal: %w", err)
	}
	res, err := p.raft.Apply(data, promptApplyTimeout)
	if err != nil {
		return fmt.Errorf("prompt: raft apply: %w", err)
	}
	// The FSM reports a refused CAS as its return VALUE, not as an
	// apply error. Ignoring this is how a rejected claim reads as
	// success — the same bug the session lease had.
	if ferr, ok := res.(error); ok && ferr != nil {
		return ferr
	}
	return nil
}
