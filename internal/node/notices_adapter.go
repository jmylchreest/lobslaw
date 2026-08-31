package node

import (
	"context"
	"strings"
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

// nightmareSource asks about memories that disagree.
//
// Owner-scoped at the source rather than filtered afterwards: the
// question quotes the memories, so a nightmare surfaced to the wrong
// person is a leak, not a mis-delivery.
//
// TIMING IS PART OF THE FEATURE. Dream runs at 02:00 and finds the
// contradiction then; asking then would be a notification at two in
// the morning about whether somebody is still vegetarian. Notices
// only ever ride out on a reply the user is already receiving, so
// there is no push path to get this wrong — but a user awake at 03:00
// would still have been asked, and being asked at 03:00 is not
// better for having been asked in-band.
//
// So the question waits for the morning, and for the first
// conversation of it.
type nightmareSource struct {
	store *memory.Store
	// tz resolves the user's own timezone, because "morning" is a
	// fact about the person, not about the machine. Falls back to the
	// cluster default and then UTC.
	tz  func(userID string) string
	now func() time.Time

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

// wakingHours is the window a question may be asked in, in the user's
// own timezone.
//
// Not "any time they are talking to us". Somebody up at 04:00 is
// awake but not receptive to being asked to adjudicate their own
// memories, and a question asked at the wrong moment is answered
// carelessly or not at all — which leaves the contradiction in place
// and spends the one chance per cycle to raise it.
const (
	wakingHourStart = 8
	wakingHourEnd   = 22
)

// lastRunCacheFor is how long the dream-run lookup is reused.
const lastRunCacheFor = 30 * time.Minute

func (n *nightmareSource) Notices(_ context.Context, principal string) ([]gateway.Notice, error) {
	if n == nil || n.store == nil || principal == "" {
		return nil, nil
	}
	if !n.awake(principal) {
		return nil, nil
	}
	if !n.dueSinceLastDream(principal) {
		return nil, nil
	}

	// Capped low. The nudge names one and counts the rest, so
	// gathering more than a handful is work whose result is a number.
	found, err := memory.UnresolvedNightmares(n.store, principal, 5)
	if err != nil {
		return nil, err
	}
	questions := make([]string, 0, len(found))
	for _, nm := range found {
		if nm.Question != "" {
			questions = append(questions, nm.Question)
		}
	}
	notices := gateway.NightmareNotice(questions)
	if len(notices) == 0 {
		return nil, nil
	}
	n.mu.Lock()
	n.askedAt[principal] = n.clock()
	n.mu.Unlock()
	return notices, nil
}

// awake reports whether it is a reasonable hour where this person is.
func (n *nightmareSource) awake(principal string) bool {
	loc := time.UTC
	if n.tz != nil {
		if l, err := time.LoadLocation(n.tz(userIDOf(principal))); err == nil {
			loc = l
		}
	}
	hour := n.clock().In(loc).Hour()
	return hour >= wakingHourStart && hour < wakingHourEnd
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
func (n *nightmareSource) dueSinceLastDream(principal string) bool {
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

func (n *nightmareSource) lastDreamRun() time.Time {
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

func (n *nightmareSource) clock() time.Time {
	if n.now != nil {
		return n.now()
	}
	return time.Now()
}

// userIDOf strips the principal prefix. Timezones are stored against
// the channel user id, and the notice addresses a principal.
func userIDOf(principal string) string {
	return strings.TrimPrefix(principal, "user:")
}
