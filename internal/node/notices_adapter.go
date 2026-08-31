package node

import (
	"context"

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
type nightmareSource struct{ store *memory.Store }

func (n nightmareSource) Notices(_ context.Context, principal string) ([]gateway.Notice, error) {
	if n.store == nil || principal == "" {
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
	return gateway.NightmareNotice(questions), nil
}
