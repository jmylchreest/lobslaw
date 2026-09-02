package node

import (
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Reading the trust floor at dispatch time.
//
// Boot-time validation is NOT here, and an earlier version of this
// file duplicated it on the mistaken belief that none existed.
// wireLLMProviders has validated every configured provider against the
// soul floor since long before the runtime check did — fatally, for
// EVERY provider, not just the primary. The duplicate was worse than
// redundant: it warned for backups, implying a leniency the real check
// does not offer.
//
// What boot validation cannot cover is the gap this accessor exists
// for. The soul is TUNABLE AT RUNTIME. An operator who raises
// min_trust_tier after the node started has already passed the boot
// check, and before the runtime check existed their change took effect
// in the system prompt and nowhere else — the providers already in the
// registry carried on serving turns at the tier they were admitted at.

// trustFloorAccessor is what the agent and the builtins read per turn.
//
// A function, not a value: the soul is tunable at runtime, so reading
// the floor once at boot would pin it — and an operator raising it
// would find the change took effect in the system prompt and not in
// the routing, which is the most misleading half-application
// available.
func (n *Node) trustFloorAccessor() func() types.TrustTier {
	return func() types.TrustTier {
		s := n.Soul()
		if s == nil {
			return types.TrustUnset
		}
		return s.Config.MinTrustTier
	}
}

// primaryProviderLabel is the provider the backup chain starts from.
//
// Extracted so boot validation and agent wiring cannot disagree about
// which provider is "the primary" — a check that validated a different
// provider from the one that runs is worse than no check, because it
// reports a clean bill of health for something it never looked at.
func (n *Node) primaryProviderLabel() string {
	if len(n.cfg.Compute.Providers) == 0 {
		return ""
	}
	if n.cfg.Compute.Roles.Main.Provider != "" {
		return n.cfg.Compute.Roles.Main.Provider
	}
	return n.cfg.Compute.Providers[0].Label
}
