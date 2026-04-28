package compute

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
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
}

// NewBuiltins returns an empty registry.
func NewBuiltins() *Builtins {
	return &Builtins{handlers: make(map[string]BuiltinFunc)}
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

// formatTimeForUser renders t in the synthetic __user_timezone (set
// by the agent from the user's preferences bucket), falling back to
// UTC when no zone is supplied or the zone is unparseable. Output is
// RFC3339 with explicit offset — unambiguous for both LLM parsing
// and human reading. Builtins that emit JSON containing time fields
// for the agent to render to the user should use this helper rather
// than calling t.Format(time.RFC3339) directly.
func formatTimeForUser(t time.Time, args map[string]string) string {
	if userTZ := strings.TrimSpace(args["__user_timezone"]); userTZ != "" {
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
