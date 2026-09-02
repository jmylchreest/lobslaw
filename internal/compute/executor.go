package compute

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/execretry"
	"github.com/jmylchreest/lobslaw/internal/hooks"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/sandbox"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// InvokeRequest is a single tool invocation. Params supplies values
// for any {placeholder} slots in the tool's ArgvTemplate — exact
// strings are substituted verbatim, preserving shell metacharacters
// as literal argument bytes.
type InvokeRequest struct {
	ToolName string
	Params   map[string]string
	Claims   *types.Claims
	TurnID   string
	// Timeout bounds the subprocess. Zero uses ExecutorConfig default.
	Timeout time.Duration
}

// InvokeResult carries the subprocess output plus status. Stdout /
// Stderr are the captured (and possibly truncated) bytes — callers
// check Truncated to know whether output was capped by MaxOutputBytes.
type InvokeResult struct {
	ExitCode  int
	Stdout    []byte
	Stderr    []byte
	Truncated bool
}

// ExecutorConfig tunes executor behaviour. Zero values take safe
// defaults.
type ExecutorConfig struct {
	// MaxOutputBytes bounds stdout and stderr separately. Prevents a
	// compromised tool from OOM-ing the agent via unbounded output.
	// Default: 10 MiB.
	MaxOutputBytes int64

	// DefaultTimeout is used when InvokeRequest.Timeout is zero.
	// Default: 30s.
	DefaultTimeout time.Duration

	// EnvWhitelist is the list of env-var names to pass through to
	// the subprocess. Any name not in this list is stripped. Empty
	// list → subprocess sees no environment at all (safe default).
	EnvWhitelist []string

	// WorkDir is the subprocess cwd. Default: os.TempDir().
	WorkDir string

	// AllowedPathRoots, if non-empty, constrains where tool
	// executables may live. The tool's Path is canonicalised via
	// filepath.EvalSymlinks and must resolve under one of these
	// roots. This defeats symlink-chasing attacks where a tool's
	// Path is replaced with a symlink pointing at /bin/rm.
	AllowedPathRoots []string

	// Sandbox, when non-nil, is applied to each subprocess via
	// sandbox.Apply — Linux namespaces, UID/GID mapping, etc. On
	// non-Linux platforms sandbox.Apply returns ErrUnsupportedPlatform
	// for any non-empty policy, which surfaces as an Invoke error so
	// operators don't silently run without the protections they asked
	// for.
	Sandbox *sandbox.Policy
}

// Executor runs tools through the compute-function pipeline:
//
//  1. Registry lookup
//  2. Path validation (resolve, anchor to allowed roots)
//  3. Policy check (tool:exec, resource=tool_name)
//  4. PreToolUse hook (may block or modify argv)
//  5. exec.Cmd with timeout, bounded output, env whitelist
//  6. PostToolUse hook
//  7. Return InvokeResult
//
// No Linux-namespace sandboxing in this layer — Phase 4.5 wraps the
// exec.Cmd with namespaces + seccomp + cgroups.
type Executor struct {
	registry ToolCatalogue
	policy   *policy.Engine
	hooks    *hooks.Dispatcher
	builtins BuiltinDispatcher
	cfg      ExecutorConfig
	logger   *slog.Logger

	// approvals holds "approved for the rest of this conversation"
	// grants. Consulted here rather than at each channel so every
	// dispatch path — builtins, skills, MCP — honours a grant the
	// user already gave. Nil grants nothing.
	approvals *SessionApprovals

	// gated maps a tool name to an EXTRA approval check run before it
	// executes. Empty by default: a deployment that never opted in
	// carries no additional check at all. See write_approval.go.
	gateMu sync.RWMutex
	gated  map[string]gatedTool
}

// SetSessionApprovals wires the session-scoped approval store. The
// channel that raised a confirmation records grants into the same
// instance; the executor is where they are spent.
func (e *Executor) SetSessionApprovals(a *SessionApprovals) { e.approvals = a }

// NewExecutor wires the dependencies. hooks may be nil; cfg zero
// fields take defaults. policy may be nil on nodes without it, in
// which case Invoke returns codes.Unimplemented-equivalent errors.
func NewExecutor(r ToolCatalogue, p *policy.Engine, h *hooks.Dispatcher, cfg ExecutorConfig, logger *slog.Logger) *Executor {
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = 10 * 1024 * 1024
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 30 * time.Second
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = os.TempDir()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Executor{registry: r, policy: p, hooks: h, cfg: cfg, logger: logger}
}

// SetBuiltins wires an in-process handler registry. Tools whose
// Path starts with "builtin:" dispatch through this registry
// instead of exec.CommandContext. Nil disables builtin dispatch
// (any builtin: tool invocation becomes ErrToolPathInvalid).
func (e *Executor) SetBuiltins(b BuiltinDispatcher) { e.builtins = b }

// Sentinel errors surfaced by Invoke so callers can branch.
var (
	ErrToolNotFound    = errors.New("tool not found")
	ErrToolPathInvalid = errors.New("tool path invalid or outside allowed roots")
	ErrPolicyDenied    = errors.New("policy denied")
	ErrMissingParam    = errors.New("missing required param")
	ErrNoPolicyEngine  = errors.New("no policy engine wired")
	ErrRequireConfirm  = errors.New("tool invocation requires confirmation")
)

// Invoke executes the requested tool end-to-end.
func (e *Executor) Invoke(ctx context.Context, req InvokeRequest) (*InvokeResult, error) {
	if req.ToolName == "" {
		return nil, fmt.Errorf("InvokeRequest: ToolName required")
	}

	tool, ok := e.registry.Get(req.ToolName)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrToolNotFound, req.ToolName)
	}

	// Sidecar tools aren't exec'd by this layer; Phase 4 doesn't
	// ship the sidecar client.
	if tool.SidecarOnly {
		return nil, fmt.Errorf("tool %q is sidecar-only; direct invocation not yet supported", tool.Name)
	}

	// The floor is evaluated BEFORE policy, deliberately. Policy is
	// operator-configurable all the way down to "allow everything",
	// so a check that ran after it could be configured away — and a
	// floor with an override flag is not a floor.
	if err := hardlineCheck(req.Params); err != nil {
		return nil, err
	}

	// Policy + PreToolUse hook fire the same way for both builtin
	// and subprocess tools — the dispatch target differs, but the
	// authorization and hook surface stays uniform so rtk-style
	// hooks see every invocation.
	if err := e.PolicyAllow(ctx, req.Claims, "tool:exec", tool.Name); err != nil {
		return nil, err
	}
	// A sensitive-but-not-secret path escalates a policy allow to a
	// confirmation. After PolicyAllow, so a path the operator's rules
	// already deny is refused rather than prompted about.
	// Honours the turn approval for the same reason policy does: this
	// is an escalate-to-human verdict, and the human just answered it.
	// Without the check the resumed call meets it again, raises another
	// prompt, and every tap produces a new one — a loop the resume
	// re-execution would drive indefinitely. PathDenied is a different
	// verdict on a different path and is not reachable from here.
	if err := hardlineConfirm(req.Params); err != nil {
		if !turnApproved(ctx, "tool:exec", tool.Name) {
			return nil, err
		}
	}
	// The per-tool gate — the memory write staging, the per-command
	// shell approval — for the same reason and in the same place: a
	// tool the operator's rules already deny is refused rather than
	// prompted about. Only tools explicitly marked have one, so a
	// deployment that never opted in pays a map lookup.
	//
	// Before dispatch, and it has to be: runBuiltin folds a builtin's
	// error into an InvokeResult and returns nil, so a confirmation
	// raised inside the builtin would become tool output the model
	// reads and the user never sees.
	if err := e.CheckGate(ctx, req.Claims, tool.Name, req.Params); err != nil {
		return nil, err
	}
	if e.hooks != nil {
		preResp, err := e.hooks.Dispatch(ctx, types.HookPreToolUse, hooks.Payload{
			"session_id":  req.TurnID,
			"tool_name":   tool.Name,
			"tool_input":  req.Params,
			"cwd":         e.cfg.WorkDir,
			"actor_scope": scopeOf(req.Claims),
		})
		if err != nil {
			return nil, err
		}
		_ = preResp
	}

	var (
		result *InvokeResult
		err    error
	)
	if name, isBuiltin := isBuiltinPath(tool.Path); isBuiltin {
		result, err = e.runBuiltin(ctx, req, name)
	} else {
		resolvedPath, rerr := resolveToolPath(tool.Path, e.cfg.AllowedPathRoots)
		if rerr != nil {
			return nil, fmt.Errorf("%w: %q: %v", ErrToolPathInvalid, tool.Path, rerr)
		}
		argv, aerr := substituteArgv(tool.ArgvTemplate, req.Params)
		if aerr != nil {
			return nil, aerr
		}
		result, err = e.runSubprocess(ctx, req, resolvedPath, argv)
	}
	if err != nil {
		return nil, err
	}

	// PostToolUse hook.
	if e.hooks != nil {
		// Cap the output handed to hooks at 4 KiB per stream so large
		// tool output doesn't bloat hook stdin. Hook dispatch is best-
		// effort observability — surfacing failures here would block
		// the tool result returning to the agent for "the audit hook
		// timed out," which is the wrong tradeoff.
		const hookOutputCap = 4 * 1024
		_, _ = e.hooks.Dispatch(ctx, types.HookPostToolUse, hooks.Payload{ //nolint:errcheck // best-effort
			"session_id":  req.TurnID,
			"tool_name":   tool.Name,
			"exit_code":   result.ExitCode,
			"stdout":      string(capBytes(result.Stdout, hookOutputCap)),
			"stderr":      string(capBytes(result.Stderr, hookOutputCap)),
			"actor_scope": scopeOf(req.Claims),
		})
	}

	return result, nil
}

// runBuiltin dispatches to an in-process handler resolved via the
// Builtins registry. Hooks still fire around this call (caller
// side), but there's no exec, no sandbox, no stderr stream —
// errors go back to the agent as a non-zero exit code + the error
// string in stderr so the agent can reason about them the same way
// it reasons about a subprocess failure.
func (e *Executor) runBuiltin(ctx context.Context, req InvokeRequest, name string) (*InvokeResult, error) {
	if e.builtins == nil {
		return nil, fmt.Errorf("%w: builtin scheme requires SetBuiltins", ErrToolPathInvalid)
	}
	fn, ok := e.builtins.Get(name)
	if !ok {
		return nil, fmt.Errorf("%w: builtin %q not registered", ErrToolPathInvalid, name)
	}
	stdout, exitCode, err := fn(ctx, req.Params)
	if err != nil {
		return &InvokeResult{
			ExitCode: exitCode,
			Stdout:   stdout,
			Stderr:   []byte(err.Error()),
		}, nil
	}
	return &InvokeResult{
		ExitCode: exitCode,
		Stdout:   stdout,
	}, nil
}

// runSubprocess performs the actual exec. Environment is built from
// EnvWhitelist only; PATH is NOT implicitly added.
func (e *Executor) runSubprocess(ctx context.Context, req InvokeRequest, path string, argv []string) (*InvokeResult, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = e.cfg.DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, path, argv...)
	cmd.Dir = e.cfg.WorkDir
	cmd.Env = buildEnv(e.cfg.EnvWhitelist)
	// WaitDelay force-closes stdio after context cancel so a child
	// process that inherited our pipes (e.g. sleep inside a shell)
	// can't stall Wait().
	cmd.WaitDelay = 500 * time.Millisecond

	if err := sandbox.Apply(cmd, e.resolvePolicy(req.ToolName)); err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}

	stdout := NewCappedBuffer(e.cfg.MaxOutputBytes)
	stderr := NewCappedBuffer(e.cfg.MaxOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := execretry.Run(runCtx, cmd)

	result := &InvokeResult{
		Stdout:    stdout.Bytes(),
		Stderr:    stderr.Bytes(),
		Truncated: stdout.truncated || stderr.truncated,
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		result.ExitCode = exitErr.ExitCode()
		return result, nil // non-zero exit is a tool outcome, not a Go error
	}
	if err != nil {
		return result, fmt.Errorf("exec %q: %w", path, err)
	}
	return result, nil
}

// resolvePolicy picks the sandbox Policy for the given tool via the
// fallback chain:
//
//  1. Tool-specific policy set on the Registry via SetPolicy.
//  2. Fleet-wide default on ExecutorConfig.Sandbox.
//  3. nil — no sandbox. sandbox.Apply is a no-op for a nil Policy.
//
// Deliberately separates the "tool has a policy" question from the
// "apply a policy" question so the Executor stays thin — Apply
// already handles nil gracefully, so resolvePolicy just returns
// whatever the chain produces.
func (e *Executor) resolvePolicy(toolName string) *sandbox.Policy {
	if p := e.registry.PolicyFor(toolName); p != nil {
		return p
	}
	return e.cfg.Sandbox
}

// CheckPolicy is the public entry point for policy evaluation
// outside the Invoke path. The agent uses this to gate skill +
// MCP dispatch (which doesn't go through the executor) so skills
// are subject to the same tool:exec policy as builtins. Returns
// ErrPolicyDenied / ErrRequireConfirm / nil identically to the
// internal PolicyAllow.
func (e *Executor) CheckPolicy(ctx context.Context, claims *types.Claims, action, resource string) error {
	return e.PolicyAllow(ctx, claims, action, resource)
}

// PolicyAllow returns nil when policy allows the invocation. Returns
// ErrPolicyDenied for deny and ErrRequireConfirm for require_confirmation
// — callers in Phase 6 will convert ErrRequireConfirm into a
// Channel.Prompt flow.
func (e *Executor) PolicyAllow(ctx context.Context, claims *types.Claims, action, resource string) error {
	if e.policy == nil {
		return ErrNoPolicyEngine
	}
	dec, err := e.policy.Evaluate(ctx, claims, action, resource)
	if err != nil {
		return fmt.Errorf("policy evaluate: %w", err)
	}
	switch dec.Effect {
	case types.EffectAllow:
		return nil
	case types.EffectRequireConfirmation:
		// A confirmation the user already gave for this conversation
		// is not asked again. Without this every approval is one-shot
		// and the same prompt returns for the same operation forever,
		// which trains the operator to approve without reading and
		// eventually to switch confirmations off.
		//
		// Only session grants are consulted here. "always" is a
		// policy allow rule, so it is already handled above by
		// Evaluate returning allow — there is nothing to check.
		// The answer the user just gave, for the turn they gave it in.
		// Without this "Approve" resolves the prompt, the turn resumes,
		// and the very same call meets the very same rule — so tapping
		// it produces another keyboard rather than the thing the user
		// approved.
		if turnApproved(ctx, action, resource) {
			e.logger.Debug("policy: approved for this turn",
				"action", action, "resource", resource)
			return nil
		}
		if e.approvals.Granted(ctx, action, resource) {
			e.logger.Debug("policy: confirmation already approved for this conversation",
				"action", action, "resource", resource)
			return nil
		}
		return fmt.Errorf("%w: %s", ErrRequireConfirm, dec.Reason)
	default:
		return fmt.Errorf("%w: %s", ErrPolicyDenied, dec.Reason)
	}
}

// resolveToolPath returns the canonicalised, symlink-resolved path
// of the tool, and verifies it's under an allowed root when roots
// are configured. Defeats several classes of attack:
//
//   - Relative paths that hit PATH lookup unexpectedly
//   - ".." traversal in the stored Path
//   - Symlink at the Path pointing at /bin/rm (or anything else)
//   - A tool registered at /usr/local/bin/x that's been replaced with
//     a symlink to /usr/bin/nc after registration (re-evaluated each
//     invocation, not just at registration time)
func resolveToolPath(path string, allowedRoots []string) (string, error) {
	if path == "" {
		return "", errors.New("empty path")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q is not absolute", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("evalsymlinks: %w", err)
	}
	// Reject any ".." after resolution. EvalSymlinks normally resolves
	// these but we belt-and-braces in case of future refactors.
	if strings.Contains(resolved, "/../") || strings.HasSuffix(resolved, "/..") {
		return "", fmt.Errorf("resolved path contains traversal segments: %q", resolved)
	}
	if len(allowedRoots) > 0 {
		if !anyRootContains(resolved, allowedRoots) {
			return "", fmt.Errorf("resolved path %q is outside allowed roots", resolved)
		}
	}
	return resolved, nil
}

// anyRootContains returns true when resolved sits under one of the
// roots. Roots are also canonicalised so symlinks in root paths don't
// break containment.
func anyRootContains(resolved string, roots []string) bool {
	for _, root := range roots {
		rootResolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		// Use filepath.Rel to check that resolved is inside rootResolved
		// without being fooled by shared prefixes ("/a" vs "/ab").
		rel, err := filepath.Rel(rootResolved, resolved)
		if err != nil {
			continue
		}
		if rel == "." || (!strings.HasPrefix(rel, "..") && rel != "") {
			return true
		}
	}
	return false
}

// substituteArgv replaces exact-match {placeholder} entries in tmpl
// with values from params. Partial string substitution is supported
// — since we pass argv as an array (no shell), metacharacters in
// substituted values are preserved as literal argument bytes.
//
// A placeholder appearing in the template must have a value in params;
// missing → ErrMissingParam. Extra keys in params are ignored (not
// required by any placeholder).
func substituteArgv(tmpl []string, params map[string]string) ([]string, error) {
	out := make([]string, len(tmpl))
	for i, segment := range tmpl {
		replaced := segment
		// Find every {key} token and substitute. Missing values are an error.
		for {
			start := strings.Index(replaced, "{")
			if start < 0 {
				break
			}
			end := strings.Index(replaced[start:], "}")
			if end < 0 {
				break
			}
			key := replaced[start+1 : start+end]
			val, ok := params[key]
			if !ok {
				return nil, fmt.Errorf("%w: %q in argv[%d]=%q", ErrMissingParam, key, i, segment)
			}
			replaced = replaced[:start] + val + replaced[start+end+1:]
		}
		out[i] = replaced
	}
	return out, nil
}

// buildEnv constructs the subprocess environment from the whitelist.
// Only the named variables leak through; default is empty env. This
// keeps secrets like API keys from accidentally reaching tools that
// don't need them.
func buildEnv(whitelist []string) []string {
	env := make([]string, 0, len(whitelist))
	for _, name := range whitelist {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// capBytes returns b if len(b) <= n, else b[:n].
func capBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}

// CappedBuffer is a bytes.Buffer that stops writing once cap is hit
// and flags itself as truncated. Writes that exceed cap still return
// nil error so the subprocess isn't killed by a "broken pipe" — we
// just drop the tail.
type CappedBuffer struct {
	buf       bytes.Buffer
	cap       int64
	truncated bool
}

// NewCappedBuffer returns a buffer that discards anything past
// limit bytes and remembers that it did.
func NewCappedBuffer(limit int64) *CappedBuffer { return &CappedBuffer{cap: limit} }

// Truncated reports whether any write was discarded because the cap
// was reached.
func (c *CappedBuffer) Truncated() bool { return c.truncated }

func (c *CappedBuffer) Write(p []byte) (int, error) {
	remaining := c.cap - int64(c.buf.Len())
	if remaining <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		c.buf.Write(p[:remaining])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

// Bytes returns the captured bytes. Caller must not retain across
// further writes (Buffer reuses its backing array).
func (c *CappedBuffer) Bytes() []byte { return c.buf.Bytes() }

// scopeOf returns the JWT scope for logging/audit attribution, or
// empty when claims are missing.
func scopeOf(c *types.Claims) string {
	if c == nil {
		return ""
	}
	return c.Scope
}
