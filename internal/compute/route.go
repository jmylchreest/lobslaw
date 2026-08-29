package compute

import (
	"context"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// A chain picks the START of the provider backup walk. Everything the
// walk already does — the trust floor at every candidate, health
// cooldowns, failure classification, a span per attempt — applies
// unchanged; the chain only decides where it begins.
//
// Later steps run as a pipeline once the turn has an answer. See
// runChainSteps.

type routeCtxKey struct{}

// Route is the turn's resolved routing decision.
type Route struct {
	// StartLabel is the provider the backup walk begins at.
	StartLabel string
	// ChainLabel is the chain that matched, "" when none did.
	ChainLabel string
	// Reason is why — "hint=deep", "complexity >= 70", "default_chain".
	// Carried so an operator can see which chain fired and why rather
	// than inferring it from which provider answered.
	Reason string
	// Steps is the full resolved chain. Only Steps[0] is dispatched
	// today; the rest are what multi-step execution will consume.
	Steps []ResolveStep
	// Judgment is the signal the decision was made on.
	Judgment Judgment
}

// WithRoute attaches a route to the context.
func WithRoute(ctx context.Context, r *Route) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, routeCtxKey{}, r)
}

// RouteFrom returns the turn's route, or nil when nothing resolved one.
// A nil route means "start where you always did".
func RouteFrom(ctx context.Context) *Route {
	r, _ := ctx.Value(routeCtxKey{}).(*Route)
	return r
}

// resolveRoute judges the turn and picks a chain.
//
// Returns nil when there is nothing to decide — no resolver wired, or
// the resolver could satisfy nobody. A nil route leaves the turn
// starting at PrimaryLabel, which is what it did before chains routed,
// so a resolver failure degrades to the old behaviour rather than
// failing the turn.
func (a *Agent) resolveRoute(ctx context.Context, req ProcessMessageRequest) *Route {
	if a.cfg.Resolver == nil {
		return nil
	}

	judgment := a.cfg.Judge.Judge(ctx, req.Message, req.Hint)

	floor := types.TrustUnset
	if a.cfg.Soul != nil {
		floor = FloorOf(a.cfg.Soul)
	}
	decision, err := a.cfg.Resolver.Resolve(ResolveRequest{
		Complexity:   judgment.Complexity,
		Domains:      judgment.Domains,
		Hint:         judgment.Hint,
		Scope:        scopeOf(req.Claims),
		MinTrustTier: floor,
	})
	if err != nil || decision == nil || len(decision.Steps) == 0 {
		// WARN, not fatal. ErrNoProvider here means no chain met the
		// floor — but the backup walk enforces the same floor at every
		// candidate, so letting the turn proceed does not lower it. It
		// will refuse for itself if nothing qualifies, with a message
		// naming the providers it considered.
		a.cfg.Logger.Warn("routing: no chain resolved; starting at the primary",
			"error", err, "complexity", judgment.Complexity, "hint", judgment.Hint)
		return nil
	}

	// NO CHAIN MATCHED IS NOT A ROUTING DECISION.
	//
	// The resolver synthesises a single-step fallback here, picking the
	// highest-trust provider and breaking ties alphabetically. That is
	// a reasonable answer to "who could serve this at all" and the
	// wrong answer to "where should this turn start" — with two
	// equal-trust providers it silently moves every unmatched turn off
	// roles.main and onto whichever label sorts first.
	//
	// So an unmatched turn starts where it always did. Routing
	// overrides the primary only when a chain actually matched.

	if decision.ChainLabel == "" {
		return nil
	}

	route := &Route{
		StartLabel: decision.Steps[0].Provider.Label,
		ChainLabel: decision.ChainLabel,
		Reason:     decision.TriggerReason,
		Steps:      decision.Steps,
		Judgment:   judgment,
	}
	a.cfg.Logger.Debug("routing: chain selected",
		"chain", route.ChainLabel,
		"reason", route.Reason,
		"start", route.StartLabel,
		"steps", len(route.Steps),
		"complexity", judgment.Complexity,
		"hint", judgment.Hint,
		"domains", judgment.Domains)
	return route
}
