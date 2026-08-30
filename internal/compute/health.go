package compute

import (
	"sync"
	"time"
)

// A failover chain tries each provider in turn, which is right the
// first time and wasteful the hundredth. A provider whose key was
// revoked at 09:00 is still first in the chain at 17:00, and every
// turn in between paid a round-trip and a timeout to rediscover that.
//
// So a failure is remembered for a while. Not in Raft: each node's
// view of whether it can reach a provider is legitimately its own —
// one node behind a broken egress proxy should not convince the
// cluster that OpenAI is down.
//
// Deliberately not a full circuit breaker. There is no half-open
// probe state machine, because the chain itself is the probe: when the
// cooldown lapses the provider is simply tried again in its normal
// position, and one success clears it. A separate probe would be a
// second thing that can be wrong about a provider's health.

// Cooldowns by failure class. A rejected credential is long because
// nothing changes until a human edits config; a 5xx is short because
// the provider is probably back before the coffee is.
const (
	cooldownTransient  = 30 * time.Second
	cooldownCredential = 15 * time.Minute
	cooldownQuota      = 10 * time.Minute

	// maxCooldown bounds the exponential backoff on repeated
	// transient failures, so a genuinely dead provider is retried
	// occasionally rather than never — a chain that has written off
	// every provider replies to nobody.
	maxCooldown = 5 * time.Minute
)

// ProviderHealth records what recently went wrong per provider, so a
// chain can skip one that is known to be failing. Safe for concurrent
// use; a nil *ProviderHealth reports everything healthy, so a caller
// with none wired behaves exactly as before.
type ProviderHealth struct {
	mu    sync.Mutex
	state map[string]*providerState
	now   func() time.Time
}

type providerState struct {
	until time.Time
	// consecutive counts transient failures in a row, for backoff.
	// Reset by any success, so a provider having a bad minute does not
	// carry that into next week.
	consecutive int
	class       FailureClass
}

// NewProviderHealth builds an empty tracker.
func NewProviderHealth() *ProviderHealth {
	return &ProviderHealth{state: map[string]*providerState{}, now: time.Now}
}

// Available reports whether a provider should be tried right now.
//
// A nil tracker says yes to everything. So does an unknown label: a
// provider nobody has failed against is healthy, which keeps a fresh
// process from being pessimistic about a chain it has never used.
func (h *ProviderHealth) Available(label string) bool {
	if h == nil || label == "" {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.state[label]
	if !ok {
		return true
	}
	return !h.now().Before(st.until)
}

// CooldownRemaining reports how long a provider stays skipped, or zero
// when it is available. For logging — an operator seeing a chain skip
// its primary wants to know for how long.
func (h *ProviderHealth) CooldownRemaining(label string) time.Duration {
	if h == nil || label == "" {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.state[label]
	if !ok {
		return 0
	}
	if d := st.until.Sub(h.now()); d > 0 {
		return d
	}
	return 0
}

// RecordFailure demotes a provider for a period chosen by why it
// failed.
//
// A permanent failure is NOT recorded. A 400 is a property of the
// request, not of the provider: demoting on one would let a single
// malformed turn take a healthy provider out of the chain for
// everybody else.
func (h *ProviderHealth) RecordFailure(label string, class FailureClass) {
	if h == nil || label == "" || class == FailurePermanent {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	st, ok := h.state[label]
	if !ok {
		st = &providerState{}
		h.state[label] = st
	}
	st.class = class
	st.consecutive++

	var d time.Duration
	switch class {
	case FailureCredential:
		d = cooldownCredential
	case FailureQuotaExhausted:
		d = cooldownQuota
	default:
		// Exponential on repeat, so a provider that fails once is back
		// in seconds and one that fails ten times in a row is not
		// retried every thirty seconds forever.
		d = min(cooldownTransient<<min(st.consecutive-1, 8), maxCooldown)
	}
	if until := h.now().Add(d); until.After(st.until) {
		st.until = until
	}
}

// RecordSuccess clears a provider's demotion.
//
// One success is enough. The alternative — requiring N in a row —
// keeps a recovered provider out of the chain for no reason, and the
// cost of being wrong is one extra failed attempt next time.
func (h *ProviderHealth) RecordSuccess(label string) {
	if h == nil || label == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.state, label)
}

// Demoted lists the currently-skipped providers with their remaining
// cooldown and the class that caused it, for operator-facing status.
func (h *ProviderHealth) Demoted() map[string]DemotionInfo {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	out := map[string]DemotionInfo{}
	for label, st := range h.state {
		if d := st.until.Sub(now); d > 0 {
			out[label] = DemotionInfo{Remaining: d, Class: st.class}
		}
	}
	return out
}

// DemotionInfo is why and for how long a provider is being skipped.
type DemotionInfo struct {
	Remaining time.Duration
	Class     FailureClass
}
