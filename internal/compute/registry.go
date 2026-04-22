package compute

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// ErrToolExists is returned by Register when a tool with the same
// name already exists. Use Replace for idempotent update semantics.
var ErrToolExists = errors.New("tool already registered")

// Registry holds the set of tools this node can invoke. Concurrent-
// safe; used by the agent loop's tool-call resolver and by the
// InvokeTool / ListTools RPC handlers.
//
// Tools are ephemeral — they re-register on every node start from
// config, plugin manifests, and skill declarations. The registry
// doesn't persist.
//
// Per-tool sandbox policies live alongside tool definitions in a
// parallel map rather than on ToolDef itself — pkg/types stays
// free of internal/sandbox imports (pkg/types is the stable public
// surface). Executor resolves which Policy to use via a fallback
// chain: tool-specific → fleet-wide default → nil.
type Registry struct {
	mu       sync.RWMutex
	tools    map[string]*types.ToolDef
	policies map[string]*sandbox.Policy
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		tools:    make(map[string]*types.ToolDef),
		policies: make(map[string]*sandbox.Policy),
	}
}

// Register adds t to the registry. Returns ErrToolExists if a tool
// with the same name already exists — callers who need overwrite
// semantics use Replace.
func (r *Registry) Register(t *types.ToolDef) error {
	if err := validateTool(t); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[t.Name]; exists {
		return fmt.Errorf("%w: %q", ErrToolExists, t.Name)
	}
	r.tools[t.Name] = cloneTool(t)
	return nil
}

// Replace registers t, overwriting any existing entry. Used during
// plugin reload where a fresh manifest should supersede whatever was
// loaded before.
func (r *Registry) Replace(t *types.ToolDef) error {
	if err := validateTool(t); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name] = cloneTool(t)
	return nil
}

// Get returns a defensive copy of the named tool, or (nil, false)
// if not registered. Copy prevents callers from mutating registry
// state through the returned pointer.
func (r *Registry) Get(name string) (*types.ToolDef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok {
		return nil, false
	}
	return cloneTool(t), true
}

// List returns all registered tools sorted by name. Deterministic
// order so a /v1/tools listing is stable between calls.
func (r *Registry) List() []*types.ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*types.ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, cloneTool(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Remove drops the tool. No error on missing — idempotent.
// Also removes any per-tool sandbox policy so the entry is fully gone.
func (r *Registry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
	delete(r.policies, name)
}

// SetPolicy attaches a sandbox Policy to the named tool. Overrides
// any previously-set policy; callers who want to explicitly mark a
// tool as "unsandboxed even though the fleet default sandboxes"
// pass an empty Policy{} (non-nil but no enforcement). Passing nil
// clears the per-tool policy so the fleet default takes over.
//
// It's valid to SetPolicy for a tool before Register — the policy
// persists and applies once the tool is registered.
func (r *Registry) SetPolicy(name string, p *sandbox.Policy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p == nil {
		delete(r.policies, name)
		return
	}
	r.policies[name] = p
}

// PolicyFor returns the per-tool sandbox Policy (or nil if none set).
// Executor uses this as the first step in the fallback chain —
// tool-specific policy takes precedence over the fleet-wide default.
// Returns nil (not an error) for unknown tools or tools without a
// policy; the Executor knows to fall through.
func (r *Registry) PolicyFor(name string) *sandbox.Policy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.policies[name]
}

// Len returns the number of registered tools.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// validateTool checks the mandatory invariants on a ToolDef:
//   - Name is required
//   - Path is required unless SidecarOnly (sidecars are reached via
//     the local sidecar gRPC endpoint, not exec)
//   - RiskTier must be one of the defined values
func validateTool(t *types.ToolDef) error {
	if t == nil {
		return errors.New("ToolDef is nil")
	}
	if t.Name == "" {
		return errors.New("ToolDef.Name is required")
	}
	if !t.SidecarOnly && t.Path == "" {
		return fmt.Errorf("ToolDef %q: Path required for non-sidecar tools", t.Name)
	}
	if !t.RiskTier.IsValid() {
		return fmt.Errorf("ToolDef %q: invalid RiskTier %q", t.Name, t.RiskTier)
	}
	return nil
}

// cloneTool does a shallow copy with deep copies of slices so
// callers can't mutate the registry through a returned pointer.
func cloneTool(t *types.ToolDef) *types.ToolDef {
	out := *t
	if t.ArgvTemplate != nil {
		out.ArgvTemplate = append([]string(nil), t.ArgvTemplate...)
	}
	if t.Capabilities != nil {
		out.Capabilities = append([]string(nil), t.Capabilities...)
	}
	return &out
}
