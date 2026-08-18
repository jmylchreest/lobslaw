package compute

import (
	"errors"
	"fmt"
)

// Role names the functional slot a caller wants a provider for.
// Config (pkg/config.RolesConfig) maps these to provider labels.
type Role string

const (
	RoleMain       Role = "main"
	RolePreflight  Role = "preflight"
	RoleReranker   Role = "reranker"
	RoleSummariser Role = "summariser"

	// RoleReview is the post-turn review fork — the pass that asks
	// whether anything about this turn is worth keeping.
	//
	// Explicit rather than "just use main", because the choice
	// determines how the conversation is replayed. On the main model
	// the fork reads a warm prefix cache and a full replay is mostly
	// cache reads; a different model cannot reuse that cache, so a
	// full replay would cold-write the whole transcript. The role is
	// what makes that derivable rather than guessed.
	RoleReview Role = "review"
)

// RoleMap is the resolved mapping from roles to LLM clients.
// Internal code asks RoleMap.For(RolePreflight) and gets a client
// to call; if no specific mapping is configured, the fallback
// chain kicks in (preflight → main, reranker → preflight →
// main, summariser → main).
type RoleMap struct {
	clients map[Role]LLMProvider
	main    LLMProvider

	// labels answers "which provider serves this role" by NAME, which
	// is what an operator asks and what list_providers reports. Held
	// beside clients rather than derived from them: two providers can
	// be the same client instance, and comparing clients to recover a
	// label would name whichever matched first.
	labels    map[Role]string
	mainLabel string
}

// NewRoleMap builds a RoleMap from explicit role-to-client
// mappings. Missing roles fall back to main per the chain above.
// Nil main returns an error — every deployment needs at least
// the main role wired.
func NewRoleMap(main LLMProvider, explicit map[Role]LLMProvider) (*RoleMap, error) {
	return NewRoleMapWithLabels(main, explicit, "", nil)
}

// NewRoleMapWithLabels is NewRoleMap plus the provider labels, so the
// map can say WHICH provider serves a role and not merely hand back a
// client. Labels are optional — a caller that omits them gets the
// same map with LabelFor returning "".
func NewRoleMapWithLabels(main LLMProvider, explicit map[Role]LLMProvider,
	mainLabel string, labels map[Role]string) (*RoleMap, error) {
	if main == nil {
		return nil, errors.New("roles: main provider required")
	}
	rm := &RoleMap{
		clients:   make(map[Role]LLMProvider),
		main:      main,
		labels:    make(map[Role]string),
		mainLabel: mainLabel,
	}
	rm.clients[RoleMain] = main
	rm.labels[RoleMain] = mainLabel
	for role, client := range explicit {
		if client != nil {
			rm.clients[role] = client
		}
	}
	for role, label := range labels {
		if label != "" {
			rm.labels[role] = label
		}
	}
	return rm, nil
}

// IsMain reports whether the provider resolved for a role is the same
// one the turn itself used.
//
// This is the whole replay policy: same model means a warm prefix
// cache and a full replay that is mostly cache reads; a different
// model cannot reuse that cache, so replaying the full transcript
// would cold-write all of it and a compact digest is the only
// affordable option.
func (rm *RoleMap) IsMain(role Role) bool {
	return rm.For(role) == rm.main
}

// For returns the provider for a role, walking the fallback
// chain. Never returns nil when the RoleMap was constructed with
// a non-nil main.
func (rm *RoleMap) For(role Role) LLMProvider {
	if c, ok := rm.clients[rm.resolve(role)]; ok {
		return c
	}
	return rm.main
}

// LabelFor names the provider that actually serves a role, following
// the same fallback chain For does.
//
// The RESOLVED answer, not the configured one. An unset preflight
// falls back to main, and reporting only what config named would tell
// an operator their fast path is unconfigured when it is in fact
// running on the main model — which is exactly the thing they are
// asking in order to fix.
//
// Empty when the map was built without labels.
func (rm *RoleMap) LabelFor(role Role) string {
	if l, ok := rm.labels[rm.resolve(role)]; ok && l != "" {
		return l
	}
	return rm.mainLabel
}

// resolve walks the fallback chain and returns the role whose
// assignment actually applies.
//
// ONE implementation, because For and LabelFor must never disagree:
// a label that names a provider other than the one the turn used is
// worse than no label at all.
func (rm *RoleMap) resolve(role Role) Role {
	if _, ok := rm.clients[role]; ok {
		return role
	}
	switch role {
	case RoleReranker:
		// Reranker is the only two-step fallback: preflight first,
		// then main.
		if _, ok := rm.clients[RolePreflight]; ok {
			return RolePreflight
		}
		return RoleMain
	case RolePreflight, RoleSummariser, RoleReview:
		// Review falls back to main, which is also the cheap case:
		// same model, warm cache, full replay.
		return RoleMain
	default:
		return RoleMain
	}
}

// FindProvider is a helper for callers that have a []ProviderConfig
// slice and need to locate the one matching a label. Returns the
// provider index + true on hit. Lets node.New stay ignorant of
// slice-walk idioms spread across wiring sites.
func FindProvider[T interface{ GetLabel() string }](providers []T, label string) (int, bool) {
	for i, p := range providers {
		if p.GetLabel() == label {
			return i, true
		}
	}
	return -1, false
}

// ErrUnknownRoleLabel is returned when the config references a
// provider label that isn't defined in [[compute.providers]].
// Surfaces as a boot-time configuration error, not a runtime
// panic.
var ErrUnknownRoleLabel = errors.New("roles: unknown provider label")

// LookupProviderLabel is a helper for wiring sites that need to
// turn a config-supplied label into a slice index. Emits
// ErrUnknownRoleLabel when the label isn't present.
func LookupProviderLabel(labels []string, label string) (int, error) {
	for i, l := range labels {
		if l == label {
			return i, nil
		}
	}
	return -1, fmt.Errorf("%w: %q (configured providers: %v)", ErrUnknownRoleLabel, label, labels)
}
