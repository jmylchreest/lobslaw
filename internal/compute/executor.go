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
	"time"

	"github.com/jmylchreest/lobslaw/internal/hooks"
	"github.com/jmylchreest/lobslaw/internal/policy"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
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
	registry *Registry
	policy   *policy.Engine
	hooks    *hooks.Dispatcher
	cfg      ExecutorConfig
	logger   *slog.Logger
}

// NewExecutor wires the dependencies. hooks may be nil; cfg zero
// fields take defaults. policy may be nil on nodes without it, in
// which case Invoke returns codes.Unimplemented-equivalent errors.
func NewExecutor(r *Registry, p *policy.Engine, h *hooks.Dispatcher, cfg ExecutorConfig, logger *slog.Logger) *Executor {
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

	resolvedPath, err := resolveToolPath(tool.Path, e.cfg.AllowedPathRoots)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %v", ErrToolPathInvalid, tool.Path, err)
	}

	argv, err := substituteArgv(tool.ArgvTemplate, req.Params)
	if err != nil {
		return nil, err
	}

	// Policy gate. tool:exec is the action; resource is the tool name.
	if err := e.policyAllow(ctx, req.Claims, "tool:exec", tool.Name); err != nil {
		return nil, err
	}

	// PreToolUse hook.
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
		// Future: handle preResp.Decision == modify to substitute argv.
		_ = preResp
	}

	result, err := e.runSubprocess(ctx, req, resolvedPath, argv)
	if err != nil {
		return nil, err
	}

	// PostToolUse hook.
	if e.hooks != nil {
		// Cap the output handed to hooks at 4 KiB per stream so large
		// tool output doesn't bloat hook stdin.
		const hookOutputCap = 4 * 1024
		_, _ = e.hooks.Dispatch(ctx, types.HookPostToolUse, hooks.Payload{
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

	stdout := &cappedBuffer{cap: e.cfg.MaxOutputBytes}
	stderr := &cappedBuffer{cap: e.cfg.MaxOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()

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

// policyAllow returns nil when policy allows the invocation. Returns
// ErrPolicyDenied for deny and ErrRequireConfirm for require_confirmation
// — callers in Phase 6 will convert ErrRequireConfirm into a
// Channel.Prompt flow.
func (e *Executor) policyAllow(ctx context.Context, claims *types.Claims, action, resource string) error {
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

// cappedBuffer is a bytes.Buffer that stops writing once cap is hit
// and flags itself as truncated. Writes that exceed cap still return
// nil error so the subprocess isn't killed by a "broken pipe" — we
// just drop the tail.
type cappedBuffer struct {
	buf       bytes.Buffer
	cap       int64
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
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
func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }

// scopeOf returns the JWT scope for logging/audit attribution, or
// empty when claims are missing.
func scopeOf(c *types.Claims) string {
	if c == nil {
		return ""
	}
	return c.Scope
}

// Guard against an unused-import warning during incremental builds
// — the proto import is reserved for future InvokeTool RPC wiring
// that plugs this executor into AgentService.InvokeTool.
var _ = lobslawv1.InvokeToolRequest{}
