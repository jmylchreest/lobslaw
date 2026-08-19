package gateway

import (
	"context"
	"strings"
	"time"
)

// Deciding whether two messages are one thought.
//
// debounce folds on a clock, which cannot tell "sunset? sunrise?" —
// the rest of the question — from "also cancel the 3pm meeting", a
// different one that happens to arrive four seconds later. It folds
// both, and the second loses.
//
// The fallback shape is hermes-agent's: try to combine with work that
// has not been answered yet, otherwise QUEUE it, never drop it, and
// tell the caller which happened. Its adapter does the same —
// agent.redirect() first, state.queued_prompts.append() if that
// fails — and reports "Redirected the active turn" or "queued for the
// next turn" rather than leaving the user guessing.

// RelatednessJudge answers whether an arriving message continues the
// messages already collected for a turn that has not started yet.
//
// Deliberately not "should I interrupt": folding here happens BEFORE
// the agent runs, so nothing has acted and there is nothing to
// interrupt. That keeps the judge's question small enough for a fast
// model to answer well.
type RelatednessJudge interface {
	// Related reports whether incoming belongs with pending.
	//
	// An error means "no opinion" and the caller folds anyway. A judge
	// that cannot answer must not be able to lose a message.
	Related(ctx context.Context, pending []string, incoming string) (bool, error)
}

// judgeTimeout bounds the judge, derived from the fold window rather
// than fixed.
//
// The window IS the budget: the caller is already waiting that long
// for messages to stop arriving, so a judge answering inside it costs
// nothing extra. A constant was wrong in both directions — 2s cut off
// a preflight model measured at 3.1s on this cluster (a "flash" model
// that still emits reasoning tokens for a one-word answer), and would
// have been needless latency on one measured at 0.64s.
//
// Deriving it also means an operator who widens the window because
// their people type slowly gives the judge the same room, without
// discovering a second knob.
func (g *TurnGate) judgeTimeout() time.Duration {
	if g.debounce > 0 {
		return g.debounce
	}
	return DefaultDebounce
}

// foldsWith asks the judge, with the fallbacks applied.
//
// Folds when there is no judge, when the judge errors, and when it
// times out. Every one of those is "we could not tell", and debounce
// is what this mode refines rather than replaces — so not being able
// to tell costs precision, never a message.
func (g *TurnGate) foldsWith(ctx context.Context, pending []string, incoming string) bool {
	if g.judge == nil {
		return true
	}
	if strings.TrimSpace(incoming) == "" || len(pending) == 0 {
		return true
	}
	jctx, cancel := context.WithTimeout(ctx, g.judgeTimeout())
	defer cancel()

	related, err := g.judge.Related(jctx, pending, incoming)
	if err != nil {
		// Debug, not warn: a judge that is briefly unavailable is a
		// normal condition for an optional refinement, and a warning
		// per message would be its own problem.
		g.log.Debug("turnqueue: relatedness judge unavailable; folding as debounce would",
			"err", err, "pending", len(pending))
		return true
	}
	return related
}
