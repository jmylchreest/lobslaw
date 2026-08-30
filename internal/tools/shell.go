package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
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

// ShellToolName is the registered name of the shell builtin, for the
// boot path that has to match an operator's policy.d/shell_command.toml
// against it.
//
// ShellToolDef and RegisterShellBuiltin still spell the name as a
// literal rather than using this constant: the tool inventory tests
// read the package's source with a regexp to learn which tools exist
// and which have builtins, and a constant is invisible to both scans.
// TestTheShellToolNameMatchesItsDefinition pins the duplication so it
// cannot drift.
const ShellToolName = "shell_command"

// ShellToolDef is separate from StdlibToolDefs because the risk
// tier is RiskIrreversible — a bash command can do anything in
// principle. What gates it is the per-command approval in
// shell_approval.go: every command is asked about, and the answer is
// remembered against that command rather than against the tool.
func ShellToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name:        "shell_command",
		Path:        compute.BuiltinScheme + "shell_command",
		Description: "Run a shell command and return stdout+stderr. Use sparingly. Commands the user has not already approved raise a confirmation they answer; ask for what you actually need rather than working around a pending approval. A small compiled-in floor (rm -rf /, fork bombs, mkfs, curl|sh) is refused outright and must not be retried or worked around. Commands run inside a filesystem sandbox that denies paths by path rather than by user, so sudo and copying elsewhere cannot widen it; when a command fails that way the result carries a 'sandbox' field naming the roots that do exist. timeout_secs bounds the run (default 30, max 300). cwd is optional. Return value includes stdout, stderr, exit_code, and truncated flag if output exceeded 256KB.",
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
	sbPolicy := buildShellPolicy()
	if sbPolicy != nil {
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

	result := map[string]any{
		"command":   cmd,
		"stdout":    string(stdoutOut),
		"stderr":    string(stderrOut),
		"exit_code": exitCode,
		"truncated": stdoutTrunc || stderrTrunc,
		"timed_out": errors.Is(runCtx.Err(), context.DeadlineExceeded),
	}
	if note := sandboxDenialNote(sbPolicy, exitCode, stderrOut); note != nil {
		result["sandbox"] = note
	}
	out, _ := json.Marshal(result)
	return out, 0, nil
}

// shellSandboxNote is attached to a failed command's result when the
// failure looks like the sandbox rather than the command.
//
// A bare "Permission denied" is indistinguishable from a Unix mode
// problem, so the model's next move is to try the same thing a
// different way — sudo, a copy, a different path — none of which can
// work, because Landlock denies by path regardless of who you are.
// Saying which roots exist turns that into either a correct retarget
// or an accurate report to the user.
type shellSandboxNote struct {
	Note     string   `json:"note"`
	Readable []string `json:"readable"`
	Writable []string `json:"writable"`
}

// sandboxDenialNote returns the note when a command that ran under a
// policy failed with something that reads like a denial. Returns nil
// otherwise — an unsandboxed run has nothing to explain, and a
// successful one needs no help.
//
// The system floor is described rather than enumerated. Listing /lib,
// /usr/bin and /etc back to the model is noise it can't act on, and
// the roots that ARE actionable — storage mounts and operator grants —
// get lost in it. It also keeps the layout we hand to the provider to
// what answering the question requires.
func sandboxDenialNote(p *sandbox.Policy, exitCode int, stderr []byte) *shellSandboxNote {
	if p == nil || exitCode == 0 || !looksLikeDenial(stderr) {
		return nil
	}
	note := &shellSandboxNote{
		Note: "This command ran inside a Landlock sandbox. Paths outside the roots below are " +
			"denied by path, whoever you are — sudo, chmod and copying elsewhere cannot get around it, " +
			"so do not retry variants of the same command. Standard system paths (the loader, " +
			"/usr/bin, /bin, /etc, /tmp and the usual devices) are readable. Beyond those, work within " +
			"the roots listed here, or tell the user which path you need and that it has to be granted " +
			"in policy.d/" + ShellToolName + ".toml.",
	}
	for _, m := range p.Mounts {
		if isShellFloorPath(m.Path) {
			continue
		}
		switch {
		case m.Write:
			note.Writable = append(note.Writable, m.Path)
		case m.Read:
			note.Readable = append(note.Readable, m.Path)
		}
	}
	for _, path := range p.AllowedPaths {
		if slices.Contains(p.ReadOnlyPaths, path) {
			note.Readable = append(note.Readable, path)
			continue
		}
		note.Writable = append(note.Writable, path)
	}
	slices.Sort(note.Readable)
	slices.Sort(note.Writable)
	return note
}

// isShellFloorPath reports whether path is one of the compiled floor
// entries, which the note summarises in prose instead of listing.
func isShellFloorPath(path string) bool {
	for _, m := range slices.Concat(shellSystemPaths, shellDevicePaths) {
		if m.Path == path {
			return true
		}
	}
	return false
}

// denialMarkers are what the kernel's EACCES/EPERM look like by the
// time they reach stderr as text. Matched against stderr only: stdout
// carrying the words "permission denied" is usually a command
// reporting on someone else's filesystem, not its own failure.
var denialMarkers = []string{
	"permission denied",
	"operation not permitted",
	"eacces",
	"eperm",
}

func looksLikeDenial(stderr []byte) bool {
	lowered := strings.ToLower(string(stderr))
	for _, marker := range denialMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
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

// shellDevicePaths is the device floor, and it is a floor rather than
// a preset reference because the shell cannot do ordinary work without
// it. `cmd 2>/dev/null` is not an exotic request; libcrypto seeds
// itself from /dev/urandom. Landlock denies anything unlisted, so
// without these entries an ordinary redirect fails with a bare
// "Permission denied" that looks like a bug in the command.
//
// /dev/tty is deliberately absent — see the `devices` preset in
// internal/sandbox/preset.go for why.
var shellDevicePaths = []sandbox.PolicyMount{
	{Path: "/dev/null", Read: true, Write: true},
	{Path: "/dev/zero", Read: true},
	{Path: "/dev/full", Read: true, Write: true},
	{Path: "/dev/random", Read: true},
	{Path: "/dev/urandom", Read: true},
}

var shellNoMountWarnOnce sync.Once

// shellPolicyOverlay is the operator's policy.d entry for
// shell_command, installed at boot. Guarded because it is read on
// every invocation and written once during wiring.
var shellPolicyOverlay struct {
	mu     sync.RWMutex
	policy *sandbox.Policy
}

// UnsupportedShellPolicyFields names the fields in p that
// shell_command cannot honour. Empty means the file is fully
// enforceable.
//
// Both of these describe security controls, so a caller that cannot
// act on the result should say so loudly rather than swallow it: an
// operator who wrote `network_filter = true` believes egress is
// restricted. The `[sandbox]` block was cut down to one key for this
// exact reason — keys that were parsed and read by nothing meant an
// operator setting network_allow_cidr restricted nothing.
func UnsupportedShellPolicyFields(p *sandbox.Policy) []string {
	if p == nil {
		return nil
	}
	var fields []string
	if p.Namespaces.Enabled() {
		fields = append(fields, "namespaces")
	}
	if p.NetworkFilter || len(p.NetworkAllowCIDR) > 0 || p.NetworkAllowDNS {
		fields = append(fields, "network_filter/network_allow_cidr/network_allow_dns")
	}
	return fields
}

// SetShellPolicyOverlay installs the operator's policy.d/shell_command
// entry.
//
// It exists because shell_command is a builtin: it dispatches
// in-process and never passes through the Executor's exec path, so a
// policy handed to Registry.SetPolicy is stored and then never
// consulted. Routing it here is what makes the file mean anything.
//
// What the overlay is for is a path outside the storage mounts that
// the operator wants the shell to reach, granted in a file instead of
// by editing Go. Three limits on what a file can do, each enforced
// rather than merely documented:
//
//   - The floor's own entries survive. A policy that dropped /lib
//     couldn't exec /bin/sh, so "operator removed the floor" has no
//     useful meaning — it just breaks the tool. Duplicates merge to
//     the most permissive access rather than the last one written.
//     An operator CAN still tighten a path nested inside a floor
//     entry, which Landlock resolves in favour of the deeper rule;
//     that is a real capability and intended.
//   - seccomp entries are added to the baseline, never substituted
//     for it, unlike an ordinary tool's policy where the file
//     describes the whole sandbox and replacing is correct.
//   - It is emphatically not a way around the hardline floor in
//     internal/policy. That refuses commands by what they do, runs
//     before any of this, and no file can switch it off.
//
// Passing nil clears the overlay.
func SetShellPolicyOverlay(p *sandbox.Policy) {
	shellPolicyOverlay.mu.Lock()
	defer shellPolicyOverlay.mu.Unlock()
	shellPolicyOverlay.policy = p
}

func shellOverlay() *sandbox.Policy {
	shellPolicyOverlay.mu.RLock()
	defer shellPolicyOverlay.mu.RUnlock()
	return shellPolicyOverlay.policy
}

// ShellPolicyOverlay returns a copy of the installed overlay, or nil
// when the operator supplied none. Exported so the boot path and debug
// output can report what shell_command is actually enforcing rather
// than what a config file was hoped to mean.
//
// A copy, not the live value: handing out a pointer to the policy the
// sandbox is built from would let any caller holding it widen the
// sandbox by appending a mount. The registry clones tool definitions
// on the way out for the same reason.
func ShellPolicyOverlay() *sandbox.Policy {
	overlay := shellOverlay()
	if overlay == nil {
		return nil
	}
	clone := *overlay
	clone.Mounts = slices.Clone(overlay.Mounts)
	clone.AllowedPaths = slices.Clone(overlay.AllowedPaths)
	clone.ReadOnlyPaths = slices.Clone(overlay.ReadOnlyPaths)
	clone.NetworkAllowCIDR = slices.Clone(overlay.NetworkAllowCIDR)
	clone.Seccomp.Deny = slices.Clone(overlay.Seccomp.Deny)
	return &clone
}

// buildShellPolicy returns the sandbox policy for shell_command:
// the shellSystemPaths + shellDevicePaths floor, the active storage
// mounts, and whatever the operator granted in policy.d on top.
//
// Returns nil when there is nothing to enforce — no mounts and no
// overlay — so existing integration tests that don't set up storage
// keep working. An overlay on its own is enough to sandbox: an
// operator who wrote the file asked for enforcement.
func buildShellPolicy() *sandbox.Policy {
	mounts := LandlockMounts()
	overlay := shellOverlay()
	if len(mounts) == 0 && overlay == nil {
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
	return composeShellPolicy(mounts, overlay)
}

// composeShellPolicy is buildShellPolicy's composition, split out from
// its two global reads so the result can be asserted without a wired
// node.
//
// The floor's own entries survive whatever the operator wrote:
// duplicates merge to the most permissive access (the documented
// composition rule) rather than letting a later entry for the same
// path replace an earlier one. What an operator CAN still do is name a
// path nested inside a floor entry, which Landlock resolves in favour
// of the deeper rule — /tmp/scratch:r under the floor's /tmp:rw gives
// a read-only /tmp/scratch. That is a tightening, and intentional.
func composeShellPolicy(mounts []sandbox.PolicyMount, overlay *sandbox.Policy) *sandbox.Policy {
	policy := &sandbox.Policy{NoNewPrivs: true}
	policy.Mounts = append(policy.Mounts, shellSystemPaths...)
	policy.Mounts = append(policy.Mounts, shellDevicePaths...)
	policy.Mounts = append(policy.Mounts, mounts...)
	if overlay != nil {
		policy.Mounts = append(policy.Mounts, overlay.Mounts...)
		// Carried rather than folded into Mounts: the loader produces
		// these two fields, and the install layer treats both inputs
		// as additive.
		policy.AllowedPaths = overlay.AllowedPaths
		policy.ReadOnlyPaths = overlay.ReadOnlyPaths
		// Union, not assignment. A policy.d file's seccomp_deny
		// REPLACES the baseline for an ordinary tool, which is the
		// documented behaviour and fine when the file describes the
		// whole sandbox. Here it would let `seccomp_deny = ["read"]`
		// quietly drop ptrace, mount, bpf and keyctl from a tool that
		// runs arbitrary commands — a narrowing dressed as a setting.
		policy.Seccomp = sandbox.DefaultSeccompPolicy.Union(overlay.Seccomp)
	}
	policy.Mounts = sandbox.MergeMounts(policy.Mounts)
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
