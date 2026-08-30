package compute

import (
	"context"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
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
//
// Declared here rather than alongside the handlers in [tools]
// because the executor dispatches through it. An interface method
// returning a named type is only satisfied by that exact type, so
// the tool package's dispatcher must return *this* declaration for
// BuiltinDispatcher to be implementable.
type BuiltinFunc func(ctx context.Context, args map[string]string) (stdout []byte, exitCode int, err error)

// isBuiltinPath returns the handler name + true if path addresses
// a builtin. Empty return with false means a normal filesystem
// path.
func isBuiltinPath(path string) (string, bool) {
	name, ok := strings.CutPrefix(path, BuiltinScheme)
	return name, ok
}

// ToolCatalogue is the executor's and the agent's view of the
// registered tools. The concrete implementation is [tools.Registry],
// which lives above this package: the tool handlers need compute's
// drivers, providers and trust floor, so the dependency can only run
// one way. Naming the three methods we actually consume keeps that
// direction honest and keeps the executor testable with a stub.
type ToolCatalogue interface {
	// Get resolves a tool by name. Returns false for an unknown or
	// disabled tool.
	Get(name string) (*types.ToolDef, bool)
	// PolicyFor returns the sandbox policy registered for a tool, or
	// nil when the tool runs under the default policy.
	PolicyFor(name string) *sandbox.Policy
	// LLMTools renders the enabled catalogue in the wire shape the
	// provider expects.
	LLMTools() []Tool
}

// BuiltinDispatcher resolves an in-process handler by name. Backed by
// [tools.Builtins].
type BuiltinDispatcher interface {
	Get(name string) (BuiltinFunc, bool)
}
