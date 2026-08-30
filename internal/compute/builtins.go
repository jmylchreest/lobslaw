package compute

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// BuiltinScheme prefixes a ToolDef.Path when the tool is dispatched
// in-process rather than as a subprocess exec. Anything else in
// Path (an absolute filesystem path like "/bin/ls") continues to go
// through the normal subprocess path.
const BuiltinScheme = "builtin:"

// BuiltinFunc implements a Go-native tool. Receives the raw LLM
// tool-call arguments (already unmarshalled from JSON) and returns
// the stdout payload + exit code. Errors surface to the agent as a
// tool failure — the caller captures them into stderr.
//
// Builtins don't see the sandbox, hooks, or subprocess plumbing.
// The policy gate still fires (same Invoke path), so a builtin can
// be allow/deny-gated identically to an exec tool.
type BuiltinFunc func(ctx context.Context, args map[string]string) (stdout []byte, exitCode int, err error)

// Builtins is the in-process tool-handler registry. Keyed by the
// portion of ToolDef.Path after "builtin:" — e.g. a tool with
// Path="builtin:current_time" dispatches to Builtins.Get("current_time").
type Builtins struct {
	mu       sync.RWMutex
	handlers map[string]BuiltinFunc

	// health is shared by every modality failover chain registered
	// here. It lives on the registry rather than being threaded
	// through five Register signatures because it is per-node state
	// with exactly the same lifetime, and because a demotion is only
	// useful if the chains share one — a provider that failed for
	// read_image is the same endpoint speak would reach.
	health *ProviderHealth

	// trustFloor reads the soul's min_trust_tier. A function rather
	// than a value because the soul is tunable at runtime: reading it
	// once at registration would pin the floor to boot, and an
	// operator raising it would find the change took effect in the
	// prompt and not in the routing.
	trustFloor func() types.TrustTier
}

// NewBuiltins returns an empty registry.
func NewBuiltins() *Builtins {
	return &Builtins{handlers: make(map[string]BuiltinFunc)}
}

// SetHealth wires the shared provider-health tracker. Nil (the
// default) reports everything healthy, so a registry without one
// behaves exactly as it did before health tracking existed.
func (b *Builtins) SetHealth(h *ProviderHealth) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.health = h
}

// Health returns the tracker, or nil.
func (b *Builtins) Health() *ProviderHealth {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.health
}

// Register errors on empty name, nil handler, or duplicate —
// builtins are boot-time wiring; duplicates indicate a config bug.
func (b *Builtins) Register(name string, fn BuiltinFunc) error {
	if name == "" {
		return errors.New("builtins: name required")
	}
	if fn == nil {
		return fmt.Errorf("builtins: %q handler is nil", name)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.handlers[name]; ok {
		return fmt.Errorf("builtins: %q already registered", name)
	}
	b.handlers[name] = fn
	return nil
}

func (b *Builtins) Get(name string) (BuiltinFunc, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	fn, ok := b.handlers[name]
	return fn, ok
}

// formatTimeForUser renders t in the turn identity's timezone (set
// by the agent from the user's preferences bucket), falling back to
// UTC when no zone is supplied or the zone is unparseable. Output is
// RFC3339 with explicit offset — unambiguous for both LLM parsing
// and human reading. Builtins that emit JSON containing time fields
// for the agent to render to the user should use this helper rather
// than calling t.Format(time.RFC3339) directly.
func formatTimeForUser(ctx context.Context, t time.Time) string {
	if userTZ := identityTimezone(ctx); userTZ != "" {
		if loc, err := time.LoadLocation(userTZ); err == nil {
			return t.In(loc).Format(time.RFC3339)
		}
	}
	return t.UTC().Format(time.RFC3339)
}

// isBuiltinPath returns the handler name + true if path addresses
// a builtin. Empty return with false means a normal filesystem
// path.
func isBuiltinPath(path string) (string, bool) {
	if !strings.HasPrefix(path, BuiltinScheme) {
		return "", false
	}
	return strings.TrimPrefix(path, BuiltinScheme), true
}

// SetTrustFloor wires the soul's min_trust_tier accessor. Nil (the
// default) means no floor, which is what a node with no SOUL.md gets
// and what every deployment had before the floor was enforced.
func (b *Builtins) SetTrustFloor(f func() types.TrustTier) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.trustFloor = f
}

// TrustFloor returns the accessor, or nil.
func (b *Builtins) TrustFloor() func() types.TrustTier {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.trustFloor
}

// identityTimezone is the turn's timezone, or empty when the turn has
// no identity attached (operator tooling, tests). Callers treat empty
// as UTC.
func identityTimezone(ctx context.Context) string {
	identity, ok := turn.IdentityFrom(ctx)
	if !ok {
		return ""
	}
	return identity.Timezone
}
