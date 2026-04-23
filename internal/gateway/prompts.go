package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Sentinel errors for the prompt flow. Callers map these to HTTP
// status codes / user-visible messages.
var (
	ErrPromptNotFound = errors.New("prompt: not found")
	ErrPromptExpired  = errors.New("prompt: expired")
	ErrPromptResolved = errors.New("prompt: already resolved")
)

// PromptDecision is how a user responded to a confirmation.
type PromptDecision int

const (
	PromptPending PromptDecision = iota
	PromptApproved
	PromptDenied
	PromptTimedOut
)

// String returns the canonical lowercase spelling for audit / JSON.
func (d PromptDecision) String() string {
	switch d {
	case PromptPending:
		return "pending"
	case PromptApproved:
		return "approved"
	case PromptDenied:
		return "denied"
	case PromptTimedOut:
		return "timed_out"
	default:
		return "unknown"
	}
}

// Prompt is one pending confirmation. Created by the channel when
// an agent turn returns NeedsConfirmation; resolved when the user
// answers (via long-poll POST for REST, callback_query for
// Telegram) or when the timeout fires.
type Prompt struct {
	// ID is the client-opaque identifier returned in the initial
	// agent response. Random + long enough to be unguessable.
	ID string

	// TurnID is the original turn this prompt blocks on, threaded
	// through the confirmation so audit logs correlate correctly.
	TurnID string

	// Reason is the human-readable explanation (e.g. "budget
	// exceeded on spend"). Rendered to the user verbatim.
	Reason string

	// Channel is "rest" | "telegram" | etc — lets audit logs show
	// which channel created the prompt. Not used for routing.
	Channel string

	// CreatedAt is the registration timestamp.
	CreatedAt time.Time

	// ExpiresAt is when the registry will auto-deny this prompt.
	ExpiresAt time.Time

	// Decision holds the resolution once the user answers (or the
	// timeout fires).
	Decision PromptDecision

	// resolved is closed when Decision transitions out of Pending.
	// Wait() blocks on it.
	resolved chan struct{}
}

// PromptRegistry holds in-flight prompts, keyed by ID. Safe for
// concurrent access. In-memory only — sufficient for single-node
// deployments; a clustered build-out would back this with the
// memory.Store (keyed by TurnID) so a different node can resolve
// the prompt created by a peer. Out of scope for Phase 6f.
type PromptRegistry struct {
	mu      sync.Mutex
	prompts map[string]*Prompt
}

// NewPromptRegistry constructs an empty registry.
func NewPromptRegistry() *PromptRegistry {
	return &PromptRegistry{prompts: make(map[string]*Prompt)}
}

// Create registers a new pending prompt and returns it. The
// ExpiresAt field is set to time.Now() + ttl; Wait will return
// PromptTimedOut if no resolution arrives before then.
//
// The returned ID is a random 32-hex-char string — long enough to
// be unguessable across a realistic number of in-flight prompts.
func (r *PromptRegistry) Create(turnID, reason, channel string, ttl time.Duration) (*Prompt, error) {
	id, err := randomHexID()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	p := &Prompt{
		ID:        id,
		TurnID:    turnID,
		Reason:    reason,
		Channel:   channel,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
		Decision:  PromptPending,
		resolved:  make(chan struct{}),
	}
	r.mu.Lock()
	r.prompts[id] = p
	r.mu.Unlock()
	// Auto-timeout fires a separate goroutine so Wait() callers
	// don't need to plumb their own deadline — the registry handles it.
	time.AfterFunc(ttl, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.transitionLocked(p, PromptTimedOut)
	})
	return p, nil
}

// Get returns a snapshot of the prompt's current state. Nil and
// ErrPromptNotFound when the ID is unknown (or was reaped).
func (r *PromptRegistry) Get(id string) (*Prompt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.prompts[id]
	if !ok {
		return nil, ErrPromptNotFound
	}
	snapshot := *p
	return &snapshot, nil
}

// Resolve transitions a prompt from Pending to the given decision.
// Returns ErrPromptResolved if the prompt has already been resolved
// (by prior call, timeout, etc) — prevents "user approves then
// denies" races from replaying. Decision must be Approved or
// Denied; PromptTimedOut is set by the internal timer only.
//
// The check-and-transition is atomic under r.mu so concurrent
// callers see exactly one winner (nil return) and all losers get
// ErrPromptResolved. A split lock would let multiple callers pass
// the Pending check and both return nil even though only one
// actually mutated state — caught by the concurrent-resolve test.
func (r *PromptRegistry) Resolve(id string, decision PromptDecision) error {
	if decision != PromptApproved && decision != PromptDenied {
		return errors.New("prompt: Resolve accepts only Approved or Denied")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.prompts[id]
	if !ok {
		return ErrPromptNotFound
	}
	if p.Decision != PromptPending {
		return ErrPromptResolved
	}
	r.transitionLocked(p, decision)
	return nil
}

// Wait blocks until the prompt resolves (user answers, timeout
// fires, or ctx cancels). Returns the final Decision. A cancelled
// ctx returns PromptPending + ctx.Err() to distinguish "I stopped
// waiting" from "resolved pending" (which can't happen).
func (r *PromptRegistry) Wait(ctx context.Context, id string) (PromptDecision, error) {
	r.mu.Lock()
	p, ok := r.prompts[id]
	if !ok {
		r.mu.Unlock()
		return PromptPending, ErrPromptNotFound
	}
	if p.Decision != PromptPending {
		d := p.Decision
		r.mu.Unlock()
		return d, nil
	}
	waitCh := p.resolved
	r.mu.Unlock()

	select {
	case <-waitCh:
	case <-ctx.Done():
		return PromptPending, ctx.Err()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return p.Decision, nil
}

// Reap drops prompts whose ExpiresAt is in the past. Called by a
// background janitor (or tests) to keep the map bounded over long
// uptime. Idempotent.
func (r *PromptRegistry) Reap() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	removed := 0
	for id, p := range r.prompts {
		// Only drop resolved/timed-out entries that have aged out;
		// Pending prompts stay until their timeout fires (the
		// AfterFunc handles that resolution).
		if p.Decision != PromptPending && now.After(p.ExpiresAt) {
			delete(r.prompts, id)
			removed++
		}
	}
	return removed
}

// transitionLocked transitions a prompt to the given decision and
// closes its resolved channel so Wait() callers unblock. Caller
// must hold r.mu. No-ops when the prompt is already past Pending
// (first writer wins). Name reflects the contract: caller holds
// the lock, not "grab the lock".
func (r *PromptRegistry) transitionLocked(p *Prompt, decision PromptDecision) {
	if p.Decision != PromptPending {
		return
	}
	p.Decision = decision
	close(p.resolved)
}

// randomHexID returns 32 hex chars (16 random bytes) — unguessable
// across any realistic in-flight set without being unwieldy in URLs.
func randomHexID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
