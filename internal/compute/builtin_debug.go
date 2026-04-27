package compute

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// DebugInspector is what the compute layer asks for operator
// introspection. The node package implements it and passes one in.
// Channels are transports — they never touch this; the agent
// invokes debug tools by name when the model (prompted by an
// operator) calls them.
type DebugInspector interface {
	DebugTools() []string
	DebugPolicyRules() []string
	DebugStorageMounts() []string
	DebugMemoryStats() map[string]int
	DebugSoul() string
	DebugRaft() map[string]any
	DebugScheduler() []map[string]any
	DebugProviders() []map[string]any
	DebugVersion() string
	DebugSandbox() map[string]any
	DebugMCP() map[string]any
}

// RegisterDebugBuiltins installs debug_* builtins. Nil inspector
// no-ops — the caller on a node without introspection wiring
// doesn't need to gate the call. Scope-level gating (who can
// invoke these) is the operator's policy responsibility: a deny
// rule against debug_* for subject!=owner keeps non-operator
// traffic out. No separate config flag — policy is the single
// gating mechanism.
func RegisterDebugBuiltins(b *Builtins, insp DebugInspector) error {
	if insp == nil {
		return nil
	}
	if err := b.Register("debug_tools", wrapDebug(func() any { return insp.DebugTools() })); err != nil {
		return err
	}
	if err := b.Register("debug_policy", wrapDebug(func() any { return insp.DebugPolicyRules() })); err != nil {
		return err
	}
	if err := b.Register("debug_storage", wrapDebug(func() any { return insp.DebugStorageMounts() })); err != nil {
		return err
	}
	if err := b.Register("debug_memory", wrapDebug(func() any { return insp.DebugMemoryStats() })); err != nil {
		return err
	}
	if err := b.Register("debug_soul", wrapDebugString(insp.DebugSoul)); err != nil {
		return err
	}
	if err := b.Register("debug_raft", wrapDebug(func() any { return insp.DebugRaft() })); err != nil {
		return err
	}
	if err := b.Register("debug_scheduler", wrapDebug(func() any { return insp.DebugScheduler() })); err != nil {
		return err
	}
	if err := b.Register("debug_providers", wrapDebug(func() any { return insp.DebugProviders() })); err != nil {
		return err
	}
	if err := b.Register("debug_version", wrapDebugString(insp.DebugVersion)); err != nil {
		return err
	}
	if err := b.Register("debug_sandbox", wrapDebug(func() any { return insp.DebugSandbox() })); err != nil {
		return err
	}
	if err := b.Register("debug_mcp", wrapDebug(func() any { return insp.DebugMCP() })); err != nil {
		return err
	}
	return nil
}

// DebugToolDefs returns the ToolDef entries for each debug
// builtin. Always RiskTier=reversible — they're read-only
// introspection. Scope gating is the operator's policy
// responsibility (deny for non-owner scopes).
func DebugToolDefs() []*types.ToolDef {
	mk := func(name, desc string) *types.ToolDef {
		return &types.ToolDef{
			Name:             name,
			Path:             BuiltinScheme + name,
			Description:      desc,
			ParametersSchema: []byte(`{"type":"object","properties":{},"additionalProperties":false}`),
			RiskTier:         types.RiskReversible,
		}
	}
	return []*types.ToolDef{
		mk("debug_tools", "List every tool registered on this node (including debug tools themselves). Takes no arguments. Useful when the operator asks 'what can you do?' or 'what tools do you have?'."),
		mk("debug_policy", "List every policy rule ID loaded on this node. Takes no arguments. Useful for operators verifying which allow/deny rules are active."),
		mk("debug_storage", "List every configured storage mount with label, backend, path, health. Takes no arguments."),
		mk("debug_memory", "Return per-bucket record counts for the Raft-backed state store (episodic, vector, scheduled tasks, policy rules, storage mounts). Takes no arguments."),
		mk("debug_soul", "Return the current SOUL.md config as JSON — name, scope, emotive dimensions, adjustments, feedback. Takes no arguments."),
		mk("debug_raft", "Return current Raft state: node ID, leader, term, peers, function list. Useful for diagnosing cluster membership and leadership."),
		mk("debug_scheduler", "List scheduled tasks with their schedule, next fire, claim state. Takes no arguments."),
		mk("debug_providers", "List configured LLM providers with label, model, endpoint (not the API key). Shows which role each serves."),
		mk("debug_version", "Return node_id + enabled functions. Takes no arguments."),
		mk("debug_sandbox", "Return kernel sandbox capabilities: whether landlock (filesystem LSM), seccomp (syscall filter), PR_SET_NO_NEW_PRIVS, and cgroup v2 are available on this host, plus whether the lobslaw daemon itself is running under a seccomp filter. Use when the operator asks 'is the sandbox active?' or wants to verify tool isolation. Takes no arguments. Present the booleans as a markdown table; call out sandbox_mode ('enforces-tools' vs 'none') prominently."),
		mk("debug_mcp", "Return live MCP registry state: per-server command/args/install spec/tool count + the full tool list under each server (qualified name + raw name + risk tier). Use when diagnosing 'why isn't tool X available' or 'did this server come up cleanly'. Complements mcp_list (which is operator-facing and doesn't expose tool names) by giving the agent its own view of what's installed."),
	}
}

func wrapDebug(fn func() any) BuiltinFunc {
	return func(_ context.Context, _ map[string]string) ([]byte, int, error) {
		v := fn()
		if v == nil {
			v = []string{}
		}
		out, err := json.Marshal(v)
		if err != nil {
			return nil, 1, err
		}
		return out, 0, nil
	}
}

func wrapDebugString(fn func() string) BuiltinFunc {
	return func(_ context.Context, _ map[string]string) ([]byte, int, error) {
		out, err := json.Marshal(map[string]any{"result": strings.TrimSpace(fn())})
		if err != nil {
			return nil, 1, err
		}
		return out, 0, nil
	}
}
