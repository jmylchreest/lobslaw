package node

import (
	"context"
	"fmt"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// botResolverAdapter turns the raft-backed registry into the narrow
// interface the turn runner asks for.
//
// The same shape as episodicIngesterAdapter beside it: compute must
// not have to know which store a bot record lives in, and the registry
// must not have to know what a turn needs from it.
type botResolverAdapter struct {
	svc *memory.BotService
}

func (a *botResolverAdapter) ResolveBot(ctx context.Context, botID string) (*compute.BotProfile, error) {
	if a == nil || a.svc == nil {
		return nil, fmt.Errorf("bots: registry not wired")
	}
	rec, err := a.svc.Get(ctx, botID)
	if err != nil {
		return nil, err
	}
	// A disabled bot is refused HERE rather than by whoever asked for
	// it. Disabling is how an operator stops a misbehaving bot, and a
	// switch that only some callers honour is not a switch.
	if !rec.GetEnabled() {
		return nil, fmt.Errorf("%w: %q", compute.ErrBotDisabled, botID)
	}
	return botProfileFrom(rec), nil
}

func botProfileFrom(rec *lobslawv1.BotRecord) *compute.BotProfile {
	if rec == nil {
		return nil
	}
	return &compute.BotProfile{
		ID:            rec.GetId(),
		DisplayName:   rec.GetDisplayName(),
		Instructions:  rec.GetInstructions(),
		Owner:         rec.GetOwner(),
		IsCoordinator: rec.GetIsCoordinator(),
		Tools:         rec.GetTools(),
		MayMessage:    rec.GetMayMessage(),
		ModelRole:     rec.GetModelRole(),
		Caps: compute.BudgetCaps{
			MaxToolCalls:   int(rec.GetBudget().GetMaxToolCalls()),
			MaxSpendUSD:    rec.GetBudget().GetMaxSpendUsd(),
			MaxEgressBytes: rec.GetBudget().GetMaxEgressBytes(),
		},
	}
}

// botResolverOrNil bridges a nil *botResolverAdapter to an
// interface-typed nil. Go's well-known gotcha: a nil pointer stored in
// an interface compares as non-nil, and the turn runner branches on
// "no resolver" to mean "run as the node default".
func botResolverOrNil(svc *memory.BotService) compute.BotResolver {
	if svc == nil {
		return nil
	}
	return &botResolverAdapter{svc: svc}
}
