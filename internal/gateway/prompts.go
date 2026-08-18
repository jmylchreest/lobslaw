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

// Prompts is the confirmation registry the channels talk to. Two
// implementations exist: the in-memory PromptRegistry below, and a
// raft-backed one for nodes that host cluster state. The interface is
// what lets a gateway on a compute-only node keep working — it has no
// local raft, so there is nothing durable for it to write to.
type Prompts interface {
	Create(NewPrompt) (*Prompt, error)
	Get(id string) (*Prompt, error)
	// Resolve records the user's answer. scope is what the button they
	// tapped offered; PromptScopeOnce is the plain "yes, this time".
	Resolve(id string, decision PromptDecision, scope PromptScope) error
	Wait(ctx context.Context, id string) (PromptDecision, error)
}

// PromptScope is how far an approval reaches.
type PromptScope int

const (
	// PromptScopeOnce covers this turn and nothing else.
	PromptScopeOnce PromptScope = iota
	// PromptScopeSession covers the rest of the conversation.
	PromptScopeSession
	// PromptScopeAlways mints a revocable policy rule.
	PromptScopeAlways
)

func (s PromptScope) String() string {
	switch s {
	case PromptScopeSession:
		return "session"
	case PromptScopeAlways:
		return "always"
	default:
		return "once"
	}
}

// ParsePromptScope reads a scope off the wire. Anything unrecognised —
// including the empty string — is "once": a typo in a REST body must
// narrow the grant, never widen it.
func ParsePromptScope(s string) PromptScope {
	switch s {
	case "session":
		return PromptScopeSession
	case "always":
		return PromptScopeAlways
	default:
		return PromptScopeOnce
	}
}

// NewPrompt is everything a confirmation needs to be answered
// somewhere other than where it was asked.
//
// A struct rather than positional arguments because the list grew past
// the point where `Create(a, b, c, ttl)` says anything at a call site,
// and because the next field to be added should not be a signature
// change in four places.
type NewPrompt struct {
	TurnID    string
	SessionID string
	Reason    string
	Channel   string
	// ChannelID is where the resolution gets delivered. Without it a
	// node that did not ask the question has no way to reply to it.
	ChannelID string
	TTL       time.Duration

	// Action and Resource name the operation being confirmed, so a
	// "session" or "always" answer records a grant that matches. Empty
	// for a budget confirmation — spend is not an operation, and a
	// button that silenced future budget warnings is the last thing an
	// operator wants on that prompt.
	Action   string
	Resource string

	// Continuation is the paused turn. Nil where the channel resumes
	// in-process and has no need to move it.
	Continuation *Continuation

	// Enrolment links this question to an operator enrolment request.
	//
	// The answer has to reach somewhere other than a paused turn:
	// there is no turn. Carried like Continuation is, so the
	// resolution path stays one path rather than growing a parallel
	// one that would need its own audience check.
	Enrolment string

	// RaisedFor is the channel-native id of the user this question is
	// being asked OF — the Telegram user id, not the canonical
	// principal, because that is what a callback arrives carrying.
	//
	// Captured when the prompt is raised rather than read off the
	// answer, for the reason the "always" grant path already gives:
	// a callback is attacker-shaped input, and the turn that
	// triggered the confirmation is not.
	RaisedFor string
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

	// SessionID is the conversation this turn belongs to, so a
	// resumed leg is appended to the right transcript.
	SessionID string

	// ChannelID is where the resolution gets delivered.
	ChannelID string

	// Action and Resource name the operation, for a scoped answer.
	Action   string
	Resource string

	// Continuation is the paused turn, when the channel stored one.
	Continuation *Continuation

	// RaisedFor is the channel-native id of the user the question was
	// asked of. Only they may answer it.
	RaisedFor string

	// Enrolment is the operator enrolment request this answers, if any.
	Enrolment string

	// Decision holds the resolution once the user answers (or the
	// timeout fires).
	Decision PromptDecision

	// Scope is how far the answer reaches. Meaningless until Decision
	// leaves Pending.
	Scope PromptScope

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

// defaultPromptTTL bounds a confirmation that arrives with no TTL of
// its own. A prompt with no expiry is a turn that waits forever.
const defaultPromptTTL = 5 * time.Minute

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
func (r *PromptRegistry) Create(np NewPrompt) (*Prompt, error) {
	id, err := randomHexID()
	if err != nil {
		return nil, err
	}
	ttl := np.TTL
	if ttl <= 0 {
		ttl = defaultPromptTTL
	}
	now := time.Now()
	p := &Prompt{
		ID:           id,
		TurnID:       np.TurnID,
		SessionID:    np.SessionID,
		Reason:       np.Reason,
		Channel:      np.Channel,
		ChannelID:    np.ChannelID,
		Action:       np.Action,
		Resource:     np.Resource,
		Continuation: np.Continuation,
		RaisedFor:    np.RaisedFor,
		Enrolment:    np.Enrolment,
		CreatedAt:    now,
		ExpiresAt:    now.Add(ttl),
		Decision:     PromptPending,
		resolved:     make(chan struct{}),
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
func (r *PromptRegistry) Resolve(id string, decision PromptDecision, scope PromptScope) error {
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
	p.Scope = scope
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
