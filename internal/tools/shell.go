package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/sandbox"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// shellDefaultTimeout is the default bounded run time. 30s is
// plenty for status/query commands; long-running ops need to
// specify timeout explicitly so the model is forced to think
// about whether a command should really run for minutes inside
// an agent turn.
const (
	shellDefaultTimeout = 30 * time.Second
	shellMaxTimeout     = 5 * time.Minute
	shellMaxOutputBytes = 256 * 1024
)

// ShellToolDef is separate from StdlibToolDefs because the risk
// tier is RiskIrreversible — a bash command can do anything in
// principle. What gates it is the per-command approval in
// shell_approval.go: every command is asked about, and the answer is
// remembered against that command rather than against the tool.
func ShellToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name:        "shell_command",
		Path:        compute.BuiltinScheme + "shell_command",
		Description: "Run a shell command and return stdout+stderr. Use sparingly. Commands the user has not already approved raise a confirmation they answer; ask for what you actually need rather than working around a pending approval. A small compiled-in floor (rm -rf /, fork bombs, mkfs, curl|sh) is refused outright and must not be retried or worked around. timeout_secs bounds the run (default 30, max 300). cwd is optional. Return value includes stdout, stderr, exit_code, and truncated flag if output exceeded 256KB.",
		ParametersSchema: []byte(`{
			"type": "object",
			"properties": {
				"command": {"type": "string", "description": "Full command string (passed via sh -c)."},
				"cwd": {"type": "string", "description": "Absolute path to run in. Default is server's workspace dir."},
				"timeout_secs": {"type": "integer", "description": "Wall-clock timeout (default 30, max 300)."}
			},
			"required": ["command"],
			"additionalProperties": false
		}`),
		// Derived rather than written into the sentence above, which
		// is where this list used to live. compute.disabled_tools can
		// switch any of them off, and a description recommending a tool
		// the model cannot call is one it keeps trying.
		RecommendTools: []string{"read_file", "list_files", "glob", "grep", "edit_file", "write_file"},
		RiskTier:       types.RiskIrreversible,
	}
}

// RegisterShellBuiltin installs shell_command. Operators who
// don't want shell access simply don't register it via config
// (once that toggle lands); today it's always registered on
// compute-enabled nodes because the per-command approval gate in
// shell_approval.go asks about every command before it runs.
func RegisterShellBuiltin(b *Builtins) error {
	return b.Register("shell_command", shellCommandBuiltin)
}

// The substring denylist that used to live here is gone.
//
// It refused thirteen shapes — sudo, ssh, curl, wget, scp, dd, mkfs
// and friends — with no way to say yes to any of them, so the answer
// to "let me run this one ssh" was to edit this file. The per-command
// gate asks about everything instead, which subsumes it: an operator
// who wants sudo answers a prompt once rather than patching Go.
//
// It is not re-added as a promptable list on top, because a code
// branch that forced confirmation regardless would mean an exact
// grant on `sudo systemctl restart nginx` could never take effect —
// the original complaint wearing a new hat.
//
// The compiled-in floor below is a different thing and stays.

func shellCommandBuiltin(ctx context.Context, args map[string]string) ([]byte, int, error) {
	cmd := strings.TrimSpace(args["command"])
	if cmd == "" {
		return nil, 2, errors.New("shell_command: command is required")
	}
	// The compiled-in floor. Policy is operator-configurable all the
	// way down to "allow everything" and an approval can say yes to
	// anything policy would ask about, so this is the one layer that
	// is neither — checking it here as well as in the executor means a
	// future caller reaching the builtin directly still hits it.
	if err := policy.CheckCommand(cmd); err != nil {
		return nil, 2, err
	}
	if err := policy.CheckCommandPaths(cmd); err != nil {
		return nil, 2, err
	}
	// And again on the canonical form, because the floor matches text:
	// its filesystem-wipe pattern catches `rm -rf /` but not
	// `rm -rf '/'`, which is the same operation with quotes on. The
	// normaliser strips exactly that difference, so checking its output
	// closes the quoting bypass without teaching the floor about shell
	// syntax.
	if key, ok := compute.NormaliseCommand(cmd); ok && key != cmd {
		if err := policy.CheckCommand(key); err != nil {
			return nil, 2, err
		}
		if err := policy.CheckCommandPaths(key); err != nil {
			return nil, 2, err
		}
	}

	timeout := shellDefaultTimeout
	if raw := args["timeout_secs"]; raw != "" {
		var n int
		if _, err := fmt.Sscanf(raw, "%d", &n); err == nil && n > 0 {
			timeout = time.Duration(n) * time.Second
		}
	}
	if timeout > shellMaxTimeout {
		timeout = shellMaxTimeout
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c := exec.CommandContext(runCtx, "/bin/sh", "-c", cmd)
	if cwd := strings.TrimSpace(args["cwd"]); cwd != "" {
		c.Dir = cwd
	}
	// PATH only — no env leakage. Operators who need more can set
	// tool-specific env via policy metadata once the Ask layer lands.
	c.Env = []string{"PATH=/usr/bin:/bin:/usr/local/bin", "HOME=/tmp"}

	// Landlock the subprocess to the active storage mounts so a
	// shell_command call can't escape mount-defined boundaries. On
	// non-Linux platforms sandbox.Apply is a no-op; we log once at
	// boot in that case so the operator knows shell runs unsandboxed.
	if sbPolicy := buildShellPolicy(); sbPolicy != nil {
		if err := sandbox.Apply(c, sbPolicy); err != nil {
			return nil, 1, fmt.Errorf("shell_command: sandbox apply: %w", err)
		}
	}

	// Capture stdout + stderr separately regardless of exit code
	// so a successful command that wrote to stderr (lots of CLIs
	// do) still surfaces it to the model.
	var stdoutBuf, stderrBuf strings.Builder
	c.Stdout = &stdoutBuf
	c.Stderr = &stderrBuf
	_ = c.Run()
	stdout := []byte(stdoutBuf.String())
	stderr := []byte(stderrBuf.String())
	exitCode := 0
	if c.ProcessState != nil {
		exitCode = c.ProcessState.ExitCode()
	}

	stdoutOut, stdoutTrunc := capBytesMax(stdout, shellMaxOutputBytes)
	stderrOut, stderrTrunc := capBytesMax(stderr, shellMaxOutputBytes)

	out, _ := json.Marshal(map[string]any{
		"command":   cmd,
		"stdout":    string(stdoutOut),
		"stderr":    string(stderrOut),
		"exit_code": exitCode,
		"truncated": stdoutTrunc || stderrTrunc,
		"timed_out": errors.Is(runCtx.Err(), context.DeadlineExceeded),
	})
	return out, 0, nil
}

func capBytesMax(b []byte, max int) ([]byte, bool) {
	if len(b) <= max {
		return b, false
	}
	return b[:max], true
}

// shellSystemPaths is the bare-minimum read-only set the shell needs
// regardless of operator mounts: the dynamic linker + libc, /usr/bin
// for shipped utilities, and /tmp for scratch. Without these the
// landlock'd shell can't even exec /bin/sh because it'd EACCES on
// /lib64/ld-linux-x86-64.so.2.
var shellSystemPaths = []sandbox.PolicyMount{
	{Path: "/lib", Read: true, Exec: true},
	{Path: "/lib64", Read: true, Exec: true},
	{Path: "/usr/lib", Read: true, Exec: true},
	{Path: "/usr/lib64", Read: true, Exec: true},
	{Path: "/usr/bin", Read: true, Exec: true},
	{Path: "/bin", Read: true, Exec: true},
	{Path: "/etc", Read: true},
	{Path: "/tmp", Read: true, Write: true},
}

var shellNoMountWarnOnce sync.Once

// buildShellPolicy returns the sandbox policy for shell_command,
// derived from the active storage mounts plus the shellSystemPaths
// floor. Returns nil when no mounts are wired (test/dev) so existing
// integration tests that don't set up storage keep working.
func buildShellPolicy() *sandbox.Policy {
	mounts := LandlockMounts()
	if len(mounts) == 0 {
		shellNoMountWarnOnce.Do(func() {
			// One-time signal: the operator hasn't wired any storage
			// mounts so shell_command falls open. This is the right
			// behaviour for test setups; flag it so prod misconfig
			// doesn't pass silently.
			// Diagnostic warning to stderr; nothing useful to do if it fails.
			_, _ = fmt.Fprintln(stderrForLog(),
				"shell_command: no storage mounts active — Landlock sandbox skipped (test/dev mode)")
		})
		return nil
	}
	policy := &sandbox.Policy{
		NoNewPrivs: true,
		Mounts:     append(append([]sandbox.PolicyMount(nil), shellSystemPaths...), mounts...),
	}
	return policy
}

// stderrForLog is the logging sink for the one-time shell-not-sandboxed
// warning. Centralised so tests can swap it; today it's just os.Stderr.
var stderrForLog = func() interface {
	Write(p []byte) (int, error)
} {
	return defaultStderr{}
}

type defaultStderr struct{}

func (defaultStderr) Write(p []byte) (int, error) {
	return fmt.Print(string(p))
}
