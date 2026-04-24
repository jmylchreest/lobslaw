package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

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
// principle. Operators are expected to layer a require_confirmation
// rule; the ask-based permission model will replace this with
// per-pattern approval once it lands.
func ShellToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name:        "shell_command",
		Path:        BuiltinScheme + "shell_command",
		Description: "Run a shell command and return stdout+stderr. Use sparingly — prefer dedicated tools (read_file, search_files, edit_file, write_file) for their use cases. Destructive or system-modifying commands (rm -rf, sudo, curl|sh, ssh) are rejected by a denylist. Compound commands (&&, ||, ;, |) are rejected unless allow_compound=true. timeout_secs bounds the run (default 30, max 300). cwd is optional. Return value includes stdout, stderr, exit_code, and truncated flag if output exceeded 256KB.",
		ParametersSchema: []byte(`{
			"type": "object",
			"properties": {
				"command": {"type": "string", "description": "Full command string (passed via sh -c)."},
				"cwd": {"type": "string", "description": "Absolute path to run in. Default is server's workspace dir."},
				"timeout_secs": {"type": "integer", "description": "Wall-clock timeout (default 30, max 300)."},
				"allow_compound": {"type": "boolean", "description": "Permit && / || / ; / | in the command."}
			},
			"required": ["command"],
			"additionalProperties": false
		}`),
		RiskTier: types.RiskIrreversible,
	}
}

// RegisterShellBuiltin installs shell_command. Operators who
// don't want shell access simply don't register it via config
// (once that toggle lands); today it's always registered on
// compute-enabled nodes because RiskIrreversible + policy
// override is the gating layer.
func RegisterShellBuiltin(b *Builtins) error {
	return b.Register("shell_command", shellCommandBuiltin)
}

// shellDenylist is the hard refusal set — commands that are almost
// always wrong inside an agent turn. Operators running lobslaw on
// a developer machine with a sandbox can relax this via config
// once that surface exists; for now the denylist is conservative.
var shellDenylist = []string{
	"rm -rf /", "rm -rf /*",
	"sudo ", "doas ",
	"curl ", // inside shell is usually the "curl|sh" shape; force the model to use fetch_url
	"wget ",
	"ssh ", "scp ",
	"dd if=", "mkfs.", "fdisk ",
	"shutdown ", "reboot ", "halt ",
	":(){:|:&};:", // classic fork bomb
}

// shellCompoundMarkers are what allow_compound gates. Spaces around
// && and || are normalised in the command string before matching.
var shellCompoundMarkers = []string{"&&", "||", ";", " | ", "|&"}

func shellCommandBuiltin(ctx context.Context, args map[string]string) ([]byte, int, error) {
	cmd := strings.TrimSpace(args["command"])
	if cmd == "" {
		return nil, 2, errors.New("shell_command: command is required")
	}
	for _, bad := range shellDenylist {
		if strings.Contains(cmd, bad) {
			return nil, 2, fmt.Errorf("shell_command: rejected — matches denylist %q", bad)
		}
	}
	allowCompound := args["allow_compound"] == "true"
	if !allowCompound {
		for _, m := range shellCompoundMarkers {
			if strings.Contains(cmd, m) {
				return nil, 2, fmt.Errorf("shell_command: rejected — %q in command (pass allow_compound=true to permit compound commands)", m)
			}
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
		"command":    cmd,
		"stdout":     string(stdoutOut),
		"stderr":     string(stderrOut),
		"exit_code":  exitCode,
		"truncated":  stdoutTrunc || stderrTrunc,
		"timed_out":  errors.Is(runCtx.Err(), context.DeadlineExceeded),
	})
	return out, 0, nil
}

func capBytesMax(b []byte, max int) ([]byte, bool) {
	if len(b) <= max {
		return b, false
	}
	return b[:max], true
}
