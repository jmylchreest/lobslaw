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
)

// RoleMap is the resolved mapping from roles to LLM clients.
// Internal code asks RoleMap.For(RolePreflight) and gets a client
// to call; if no specific mapping is configured, the fallback
// chain kicks in (preflight → main, reranker → preflight →
// main, summariser → main).
type RoleMap struct {
	clients map[Role]LLMProvider
	main    LLMProvider
}

// NewRoleMap builds a RoleMap from explicit role-to-client
// mappings. Missing roles fall back to main per the chain above.
// Nil main returns an error — every deployment needs at least
// the main role wired.
func NewRoleMap(main LLMProvider, explicit map[Role]LLMProvider) (*RoleMap, error) {
	if main == nil {
		return nil, errors.New("roles: main provider required")
	}
	rm := &RoleMap{
		clients: make(map[Role]LLMProvider),
		main:    main,
	}
	rm.clients[RoleMain] = main
	for role, client := range explicit {
		if client != nil {
			rm.clients[role] = client
		}
	}
	return rm, nil
}

// For returns the provider for a role, walking the fallback
// chain. Never returns nil when the RoleMap was constructed with
// a non-nil main.
func (rm *RoleMap) For(role Role) LLMProvider {
	if c, ok := rm.clients[role]; ok {
		return c
	}
	switch role {
	case RolePreflight:
		return rm.main
	case RoleReranker:
		if c, ok := rm.clients[RolePreflight]; ok {
			return c
		}
		return rm.main
	case RoleSummariser:
		return rm.main
	default:
		return rm.main
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
