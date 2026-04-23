package compute

import (
	"errors"
	"fmt"

	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// ErrNoProvider is returned by Resolver.Resolve when the caller's
// trust-tier floor (or a chain's floor) can't be satisfied by any
// available provider. Callers surface this to the user as "no
// provider meets your trust requirement" — not a transient error;
// configuration change is needed.
var ErrNoProvider = errors.New("no provider meets the trust-tier floor")

// ResolveRequest is the decision input: what complexity + domain
// hint was derived from the user's turn, which scope it runs in,
// and the minimum trust tier the caller insists on.
type ResolveRequest struct {
	// Complexity is a heuristic score (0-100) from the ComplexityEstimator.
	// Chains with a MinComplexity trigger match when Complexity >= trigger.
	Complexity int

	// Domains are free-form tags surfaced by the complexity estimator
	// or the user's claims — e.g. "code", "finance", "legal". A chain
	// with a Domains trigger matches when any domain overlaps.
	Domains []string

	// Scope is the caller's security scope (e.g. "alice", "team-x").
	// Passed through into the ResolveDecision so downstream billing/
	// auditing has it; NOT used for matching today (chain triggers
	// don't filter by scope).
	Scope string

	// MinTrustTier is the absolute floor for this turn. A provider
	// with TrustTier < MinTrustTier is rejected even if the chain
	// would otherwise allow it.
	MinTrustTier types.TrustTier
}

// ResolveDecision is the output — a concrete chain the caller can
// execute (possibly a degenerate single-step chain when the resolver
// picked a direct provider, not a multi-step chain).
type ResolveDecision struct {
	// ChainLabel is the matched chain's label ("" for the default
	// fallback or a synthesised single-step).
	ChainLabel string

	// Steps are the resolved provider configs in execution order.
	// For a simple provider pick, len(Steps) == 1.
	Steps []ResolveStep

	// TriggerReason is a short human-readable explanation of why
	// this chain matched — useful in audit logs + debug output
	// ("complexity >= 70", "domain=finance", "default fallback").
	TriggerReason string
}

// ResolveStep is one concrete provider invocation in a chain.
// Copies the relevant ProviderConfig fields plus the chain-step
// metadata (role, prompt template) for downstream use.
type ResolveStep struct {
	// Provider is the resolved *config.ProviderConfig. The caller
	// treats this as read-only — the resolver copies it in so
	// configuration mutations don't leak into in-flight turns.
	Provider config.ProviderConfig

	// Role is the chain-step role ("primary", "reviewer", etc.) —
	// used by the agent loop to thread outputs correctly. "primary"
	// is the default for synthesised single-step chains.
	Role string

	// PromptTemplate is the chain-step's template for the reviewer
	// / primary-handoff pattern. Empty for single-step chains.
	PromptTemplate string
}

// Resolver selects a provider chain for each turn based on the
// configured Providers + Chains + DefaultChain. Immutable after
// construction — safe to share across goroutines.
type Resolver struct {
	// providers is a by-label lookup built from the config. The
	// slice order doesn't matter after indexing; config.Validate
	// rejects duplicate labels.
	providers map[string]config.ProviderConfig

	// chains is the ordered list of ChainConfigs exactly as the
	// operator wrote them. Order is meaningful: the first chain
	// whose trigger matches the request wins. Operators tune
	// priority by reordering.
	chains []config.ChainConfig

	// defaultChain is the fallback applied when no other chain
	// triggers. Empty string means "no default" — in which case the
	// resolver returns a single-step chain pointing at the first
	// provider that meets the trust floor.
	defaultChain string
}

// NewResolver constructs a Resolver from compute config. Validates
// cross-references — every chain step must name a known provider,
// every chain label and provider label must be unique, default_chain
// (if set) must exist.
//
// Returns an error with all problems found (rather than the first)
// so operators fixing a broken config.toml see the full picture.
func NewResolver(cfg *config.ComputeConfig) (*Resolver, error) {
	if cfg == nil {
		return nil, errors.New("ComputeConfig is nil")
	}

	providers := make(map[string]config.ProviderConfig, len(cfg.Providers))
	var problems []string

	for _, p := range cfg.Providers {
		if p.Label == "" {
			problems = append(problems, "provider with empty label")
			continue
		}
		if _, dup := providers[p.Label]; dup {
			problems = append(problems, fmt.Sprintf("duplicate provider label %q", p.Label))
			continue
		}
		if !p.TrustTier.IsValid() {
			problems = append(problems, fmt.Sprintf("provider %q has invalid trust_tier %q", p.Label, p.TrustTier))
		}
		providers[p.Label] = p
	}

	chainLabels := make(map[string]struct{}, len(cfg.Chains))
	for _, ch := range cfg.Chains {
		if ch.Label == "" {
			problems = append(problems, "chain with empty label")
			continue
		}
		if _, dup := chainLabels[ch.Label]; dup {
			problems = append(problems, fmt.Sprintf("duplicate chain label %q", ch.Label))
		}
		chainLabels[ch.Label] = struct{}{}
		if len(ch.Steps) == 0 {
			problems = append(problems, fmt.Sprintf("chain %q has no steps", ch.Label))
		}
		for i, step := range ch.Steps {
			if step.Provider == "" {
				problems = append(problems, fmt.Sprintf("chain %q step %d has empty provider", ch.Label, i))
				continue
			}
			if _, ok := providers[step.Provider]; !ok {
				problems = append(problems, fmt.Sprintf("chain %q step %d references unknown provider %q", ch.Label, i, step.Provider))
			}
		}
		if ch.MinTrustTier != "" && !ch.MinTrustTier.IsValid() {
			problems = append(problems, fmt.Sprintf("chain %q has invalid min_trust_tier %q", ch.Label, ch.MinTrustTier))
		}
	}

	if cfg.DefaultChain != "" {
		if _, ok := chainLabels[cfg.DefaultChain]; !ok {
			problems = append(problems, fmt.Sprintf("default_chain %q is not a defined chain", cfg.DefaultChain))
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("compute config invalid: %v", problems)
	}

	return &Resolver{
		providers:    providers,
		chains:       append([]config.ChainConfig(nil), cfg.Chains...),
		defaultChain: cfg.DefaultChain,
	}, nil
}

// Resolve picks a provider chain for req. Algorithm (from PLAN.md
// Phase 5.1):
//
//  1. Walk chains in order. The first chain whose trigger matches
//     the request wins.
//  2. If no chain matched and default_chain is configured, use it.
//  3. If no chain matches and there's no default, synthesise a
//     single-step chain pointing at the highest-trust-tier provider
//     that meets MinTrustTier.
//  4. For the picked chain, verify every step's provider satisfies
//     the chain's MinTrustTier AND the request's MinTrustTier. If
//     any step fails, return ErrNoProvider — the chain as a whole
//     doesn't meet the floor.
//
// Returns ErrNoProvider wrapped with context when nothing resolves.
func (r *Resolver) Resolve(req ResolveRequest) (*ResolveDecision, error) {
	if !req.MinTrustTier.IsValid() {
		// Empty is treated as "no floor" — public is the most permissive.
		req.MinTrustTier = types.TrustPublic
	}

	for _, ch := range r.chains {
		if !triggerMatches(ch.Trigger, req) {
			continue
		}
		decision, err := r.buildDecision(ch, req, triggerReason(ch.Trigger, req))
		if err != nil {
			// Chain matched but didn't meet trust floor — keep trying
			// the rest of the chain list rather than falling straight
			// to default. Some operators rely on multiple overlapping
			// triggers for fallback.
			continue
		}
		return decision, nil
	}

	if r.defaultChain != "" {
		for _, ch := range r.chains {
			if ch.Label == r.defaultChain {
				return r.buildDecision(ch, req, "default_chain")
			}
		}
	}

	// No chain fit — synthesise a single-step chain using the highest-
	// trust provider that meets the floor. Deterministic tiebreak:
	// alphabetical by label so test output doesn't flutter.
	picked := r.pickHighestTrustProvider(req.MinTrustTier)
	if picked == nil {
		return nil, fmt.Errorf("%w: floor=%s", ErrNoProvider, req.MinTrustTier)
	}
	return &ResolveDecision{
		ChainLabel: "",
		Steps: []ResolveStep{
			{Provider: *picked, Role: "primary"},
		},
		TriggerReason: "no chain matched; picked highest-trust provider at or above floor",
	}, nil
}

// buildDecision materialises a ResolveDecision from a matched chain.
// Every step's provider is verified against the chain's MinTrustTier
// and the request's MinTrustTier. If any step fails the floor, the
// whole chain is rejected (a chain is atomic — a "reviewer step"
// using a weaker provider leaks data to that weaker tier).
func (r *Resolver) buildDecision(ch config.ChainConfig, req ResolveRequest, reason string) (*ResolveDecision, error) {
	floor := strongerFloor(ch.MinTrustTier, req.MinTrustTier)

	steps := make([]ResolveStep, 0, len(ch.Steps))
	for i, s := range ch.Steps {
		p, ok := r.providers[s.Provider]
		if !ok {
			return nil, fmt.Errorf("chain %q step %d: provider %q missing (config drift?)", ch.Label, i, s.Provider)
		}
		if !p.TrustTier.AtLeast(floor) {
			return nil, fmt.Errorf("%w: chain %q step %d provider %q trust=%s < floor=%s",
				ErrNoProvider, ch.Label, i, s.Provider, p.TrustTier, floor)
		}
		role := s.Role
		if role == "" {
			if i == 0 {
				role = "primary"
			} else {
				role = fmt.Sprintf("step-%d", i)
			}
		}
		steps = append(steps, ResolveStep{
			Provider:       p,
			Role:           role,
			PromptTemplate: s.PromptTemplate,
		})
	}

	return &ResolveDecision{
		ChainLabel:    ch.Label,
		Steps:         steps,
		TriggerReason: reason,
	}, nil
}

// pickHighestTrustProvider returns the strongest-trust provider
// meeting the floor. Tiebreak: alphabetical label. nil if none
// qualifies.
func (r *Resolver) pickHighestTrustProvider(floor types.TrustTier) *config.ProviderConfig {
	if !floor.IsValid() {
		floor = types.TrustPublic
	}
	var best *config.ProviderConfig
	for label := range r.providers {
		p := r.providers[label]
		if !p.TrustTier.AtLeast(floor) {
			continue
		}
		if best == nil {
			// Deref-safe copy to local; then take its address.
			copy := p
			best = &copy
			continue
		}
		if p.TrustTier.AtLeast(best.TrustTier) && p.TrustTier != best.TrustTier {
			copy := p
			best = &copy
		} else if p.TrustTier == best.TrustTier && p.Label < best.Label {
			copy := p
			best = &copy
		}
	}
	return best
}

// triggerMatches returns true when the chain's trigger block accepts
// the request. An `always = true` trigger matches unconditionally;
// complexity is a floor, domain is any-overlap.
func triggerMatches(t config.ChainTriggerConfig, req ResolveRequest) bool {
	if t.Always {
		return true
	}
	if t.MinComplexity > 0 {
		if req.Complexity < t.MinComplexity {
			return false
		}
		// Complexity-only triggers match if complexity is high enough.
		if len(t.Domains) == 0 {
			return true
		}
	}
	if len(t.Domains) > 0 {
		if !anyDomainOverlap(t.Domains, req.Domains) {
			return false
		}
		return true
	}
	// No trigger fields set → this chain never matches automatically.
	// (Use default_chain for the "no-trigger fallback" case.)
	return false
}

// triggerReason returns a short diagnostic string explaining which
// trigger field matched, for logs and audit.
func triggerReason(t config.ChainTriggerConfig, req ResolveRequest) string {
	switch {
	case t.Always:
		return "always=true"
	case t.MinComplexity > 0 && len(t.Domains) > 0:
		return fmt.Sprintf("complexity>=%d and domains overlap", t.MinComplexity)
	case t.MinComplexity > 0:
		return fmt.Sprintf("complexity>=%d (req=%d)", t.MinComplexity, req.Complexity)
	case len(t.Domains) > 0:
		return fmt.Sprintf("domain-overlap=%v", overlapDomains(t.Domains, req.Domains))
	default:
		return "trigger matched with no specific rule set"
	}
}

// anyDomainOverlap returns true if a and b share any element.
// Case-sensitive — domains are operator-chosen tags, not free text.
func anyDomainOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, x := range a {
		set[x] = struct{}{}
	}
	for _, x := range b {
		if _, ok := set[x]; ok {
			return true
		}
	}
	return false
}

// overlapDomains returns the intersection — used to surface which
// specific tag triggered the match in diagnostics.
func overlapDomains(a, b []string) []string {
	set := make(map[string]struct{}, len(a))
	for _, x := range a {
		set[x] = struct{}{}
	}
	var out []string
	for _, x := range b {
		if _, ok := set[x]; ok {
			out = append(out, x)
		}
	}
	return out
}

// strongerFloor picks the more restrictive of two trust floors.
// Empty string is treated as "public" (most permissive).
func strongerFloor(a, b types.TrustTier) types.TrustTier {
	if !a.IsValid() {
		a = types.TrustPublic
	}
	if !b.IsValid() {
		b = types.TrustPublic
	}
	if a.AtLeast(b) {
		return a
	}
	return b
}
