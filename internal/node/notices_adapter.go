package node

import (
	"context"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// pendingReviewSource counts what is waiting for a decision.
//
// Both halves, because they are one queue to the person draining it: a
// PROPOSED artefact and a refinement staged against a live one are
// both "somebody has to look at this", and the second is invisible in
// a state filter because the record itself is ACTIVE.
type pendingReviewSource struct{ store *memory.SelfTaughtStore }

func (p pendingReviewSource) Notices(_ context.Context, principal string) ([]gateway.Notice, error) {
	if p.store == nil {
		return nil, nil
	}
	live, err := p.store.List(memory.SelfTaughtQuery{})
	if err != nil {
		return nil, err
	}
	var proposals, refinements int
	for _, rec := range live {
		// Owner-scoped, like every other read. Somebody permitted to
		// receive notices is not thereby permitted to learn what a
		// different principal has pending — and an artefact with no
		// owner is nobody's private business, so it counts for
		// everyone.
		if rec.GetOwner() != "" && rec.GetOwner() != principal {
			continue
		}
		if rec.GetState() == lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_PROPOSED {
			proposals++
		}
		if rec.GetPending() != nil {
			refinements++
		}
	}
	return gateway.PendingReviewNotice(proposals, refinements), nil
}

// challengeSource asks about memories that disagree.
//
// Owner-scoped at the source rather than filtered afterwards: the
// question quotes the memories, so a challenge surfaced to the wrong
// person is a leak, not a mis-delivery.
//
// TIMING IS PART OF THE FEATURE. Dream finds the contradiction at
// 02:00 and says nothing. This is the first of the two ways it comes
// up: the next time the user says something. The other is a scheduled
// message, for the user who does not — see scheduleDreamChallenges.
type challengeSource struct {
	store *memory.Store
	now   func() time.Time

	mu sync.Mutex
	// askedAt is when each principal was last asked.
	//
	// Per-process, deliberately, like the notice interval it sits
	// beside: the cost of losing it is one repeated question after a
	// restart, which is a different order of thing from a permission
	// surviving where it should not, and does not earn a raft round
	// trip on the reply path.
	askedAt map[string]time.Time
	// lastRun caches when dream last ran, so the lookup is not a
	// prefix scan on every turn.
	lastRun     time.Time
	lastRunRead time.Time
}

// lastRunCacheFor is how long the dream-run lookup is reused.
const lastRunCacheFor = 30 * time.Minute

func (n *challengeSource) Notices(_ context.Context, principal string) ([]gateway.Notice, error) {
	if n == nil || n.store == nil || principal == "" {
		return nil, nil
	}
	if !n.dueSinceLastDream(principal) {
		return nil, nil
	}

	// Capped low. The nudge names one and counts the rest, so
	// gathering more than a handful is work whose result is a number.
	found, err := memory.UnresolvedChallenges(n.store, principal, 5)
	if err != nil {
		return nil, err
	}
	questions := make([]string, 0, len(found))
	for _, c := range found {
		if c.Question != "" {
			questions = append(questions, c.Question)
		}
	}
	notices := gateway.DreamChallengeNotice(questions)
	if len(notices) == 0 {
		return nil, nil
	}
	n.mu.Lock()
	n.askedAt[principal] = n.clock()
	n.mu.Unlock()
	return notices, nil
}

// dueSinceLastDream reports whether this principal has been asked
// since the last dream pass.
//
// ONE ASK PER CYCLE, not one per day and not one per conversation. A
// contradiction that survived another night has earned another
// mention; one raised this morning and not yet answered has not, and
// repeating it before dream has looked again would be nagging about
// something nothing has re-examined.
//
// A node that has never dreamt has no cycle to wait for, so the first
// ask is allowed and the next waits for a pass.
func (n *challengeSource) dueSinceLastDream(principal string) bool {
	n.mu.Lock()
	asked, everAsked := n.askedAt[principal]
	n.mu.Unlock()
	if !everAsked {
		return true
	}
	last := n.lastDreamRun()
	if last.IsZero() {
		return false
	}
	return asked.Before(last)
}

func (n *challengeSource) lastDreamRun() time.Time {
	now := n.clock()
	n.mu.Lock()
	if !n.lastRunRead.IsZero() && now.Sub(n.lastRunRead) < lastRunCacheFor {
		defer n.mu.Unlock()
		return n.lastRun
	}
	n.mu.Unlock()

	at, err := memory.LastDreamRun(n.store)
	if err != nil {
		return time.Time{}
	}
	n.mu.Lock()
	n.lastRun, n.lastRunRead = at, now
	n.mu.Unlock()
	return at
}

func (n *challengeSource) clock() time.Time {
	if n.now != nil {
		return n.now()
	}
	return time.Now()
}
