package tools

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/jmylchreest/lobslaw/internal/compute"
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
	// disabled are glob patterns that suppress registration entirely.
	// See SetDisabled.
	disabled []string
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
	// Silently, and not as an error: every caller's correct response
	// to "the operator disabled this" is to carry on, so returning an
	// error would mean each of them re-deciding that and one of them
	// eventually failing boot over a tool nobody wanted.
	if matchesAny(r.disabled, t.Name) {
		return nil
	}
	if _, exists := r.tools[t.Name]; exists {
		return fmt.Errorf("%w: %q", ErrToolExists, t.Name)
	}
	r.tools[t.Name] = cloneTool(t)
	return nil
}

// RegisterExternal is the entry point for tools whose definition
// originates outside lobslaw's own code base — skills loaded from
// manifests, MCP tools synthesised from server discovery, future
// plugin formats. It rejects any tool whose Path uses BuiltinScheme,
// preventing a skill / MCP from impersonating a builtin and
// short-circuiting the in-process Builtins dispatcher (which has
// privileged access to memory + raft + all the things builtins
// rightly do).
//
// Builtins use Register directly (BuiltinScheme path is fine
// because lobslaw's own code constructed the path). Untrusted
// sources MUST go through RegisterExternal.
//
// Name collisions still surface via ErrToolExists — a skill named
// "current_time" can't shadow the builtin if the builtin
// registered first (which it does, since wireCompute runs before
// the skill registry watcher).
func (r *Registry) RegisterExternal(t *types.ToolDef) error {
	if err := validateExternalTool(t); err != nil {
		return err
	}
	return r.Register(t)
}

// validateExternalTool enforces invariants on tools registered from
// outside lobslaw's own code base. Any failure here aborts the
// registration before it can poison the registry.
func validateExternalTool(t *types.ToolDef) error {
	if t == nil {
		return errors.New("tool: nil ToolDef")
	}
	if t.Path == "" {
		return fmt.Errorf("tool %q: external registration requires a non-empty Path (skill handler path or 'mcp:<server>:<tool>' identifier)", t.Name)
	}
	if strings.HasPrefix(t.Path, compute.BuiltinScheme) {
		return fmt.Errorf("tool %q: external registration cannot use the %q path scheme (reserved for in-process builtins)", t.Name, compute.BuiltinScheme)
	}
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
	if matchesAny(r.disabled, t.Name) {
		return nil
	}
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
	return r.withCrossRefsLocked(t), true
}

// List returns all registered tools sorted by name. Deterministic
// order so a /v1/tools listing is stable between calls.
func (r *Registry) List() []*types.ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*types.ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, r.withCrossRefsLocked(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LLMTools renders every registered tool as the function-calling
// shape the LLM client expects. Tools without a ParametersSchema
// are rendered with an empty-object schema so the model can still
// call them (no args). Used by channel handlers to populate
// compute.ProcessMessageRequest.Tools.
func (r *Registry) LLMTools() []compute.Tool {
	defs := r.List()
	out := make([]compute.Tool, 0, len(defs))
	for _, d := range defs {
		schema := d.ParametersSchema
		if len(schema) == 0 {
			schema = []byte(`{"type":"object","properties":{}}`)
		}
		out = append(out, compute.Tool{
			Name:        d.Name,
			Description: d.Description,
			Parameters:  schema,
		})
	}
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
	if t.ParametersSchema != nil {
		out.ParametersSchema = append([]byte(nil), t.ParametersSchema...)
	}
	return &out
}

// --- disabled tools ---------------------------------------------------

// DefaultDisabledTools is what a deployment that has said nothing gets.
//
// `remote_*` is off because those tools run commands on a machine this
// process does not control, and "which machines exist" is not a
// question the agent's absence of configuration should answer.
//
// `debug_*` is off because the eleven of them describe the deployment
// rather than act on it — debug_soul returns the soul text,
// debug_providers the configured endpoints and models, debug_policy
// the rules that gate every other tool. They are an operator's
// introspection surface, and an operator can ask for them by name; on
// by default they are eleven descriptions in the tool list of every
// turn, any of which a sufficiently steered model can be talked into
// reciting into whatever channel it is answering.
//
// Every other builtin is available by virtue of running the binary.
// Reaching off the box and describing the box are the two cases where
// arriving switched on is the wrong default.
//
// Turning either on is one line the operator writes knowingly, which
// is the entire point. Note that disabled_tools REPLACES this list
// rather than adding to it, so a deployment that wants debug_* on and
// remote_* off writes disabled_tools = ["remote_*"].
var DefaultDisabledTools = []string{"remote_*", "debug_*"}

// SetDisabled installs the glob patterns that suppress tool
// registration. Matched against the tool NAME with path.Match, so
// "remote_*" covers remote_ssh and anything the family grows.
//
// It lives on the registry rather than in the builtin wiring because
// the registry is the only place every source passes through. A skill
// manifest or an MCP server can declare a tool called anything it
// likes, including a name in a family the operator disabled; gating in
// wireX would cover the builtins and quietly miss those.
//
// Call before registration. Patterns set afterwards do not evict what
// is already there — the agent would have seen the tool in its list for
// the turns in between, and a tool that half-exists is worse than one
// that either does or does not.
func (r *Registry) SetDisabled(patterns []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disabled = append([]string(nil), patterns...)
}

// Disabled reports whether name matches a disable pattern. Exported so
// wiring can skip the expensive part — resolving an SSH key, dialling a
// server — for a tool that is not going to be registered anyway.
//
// An invalid pattern MATCHES NOTHING rather than everything. A typo in
// a disable list should not silently strip the agent of every tool it
// has; the operator will see the tool still present and fix the
// pattern, which is a recoverable mistake in a way the reverse is not.
func (r *Registry) Disabled(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return matchesAny(r.disabled, name)
}

func matchesAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if p == name {
			return true
		}
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

// --- cross-references -------------------------------------------------

// withCrossRefsLocked clones a tool and appends its recommend/avoid
// lines, resolved against what is registered right now.
//
// Rendered on the way OUT rather than at registration, and that is not
// a style choice: at registration time the tools a definition points
// at may not exist yet. shell_command recommends glob; whichever
// registers first would render an empty list. Resolving at read time
// has no ordering to get wrong, and costs a couple of string joins on
// a call that already clones every tool.
//
// The caller holds at least RLock.
func (r *Registry) withCrossRefsLocked(t *types.ToolDef) *types.ToolDef {
	out := cloneTool(t)
	suffix := r.crossRefLine("Prefer these where they fit", t.RecommendTools) +
		r.crossRefLine("Do not use these in its place", t.AvoidTools)
	if suffix != "" {
		out.Description = strings.TrimRight(out.Description, " ") + suffix
	}
	return out
}

// crossRefLine renders one list, or nothing.
//
// Nothing is the important case. A tool whose every recommendation is
// disabled must produce NO sentence at all — "Prefer these where they
// fit:" with an empty list tells the model something was withheld and
// invites it to guess what. Same for avoid.
//
// Names are kept in the order the author wrote them. They are a
// preference ordering ("try glob, then grep"), and sorting would throw
// that away for tidiness nobody asked for.
func (r *Registry) crossRefLine(lead string, names []string) string {
	if len(names) == 0 {
		return ""
	}
	live := make([]string, 0, len(names))
	for _, n := range names {
		// Registered, not merely known: a tool disabled by
		// compute.disabled_tools is one the model cannot call, and
		// naming it is the bug this exists to fix.
		if _, ok := r.tools[n]; ok {
			live = append(live, n)
		}
	}
	if len(live) == 0 {
		return ""
	}
	return " " + lead + ": " + strings.Join(live, ", ") + "."
}
