package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/textutil"
)

// The declarative secret provider: a configured argv, its stdout.
//
// This is what makes "any local vault" true without a Go file per tool.
// pass, gopass, sops, age, systemd-creds and every vendor CLI nobody
// has written a driver for are all the same shape — run a command with
// the item path in it, read one line back.
//
// It runs an ARGV, never a shell string. `sh -c` would make a secret
// path with a space in it into a second command, and the two existing
// places in this tree that spawn operator-configured processes —
// internal/mcp/loader.go and internal/skills/invoker.go — both take
// argv for the same reason.
//
// On trust: this runs an arbitrary command from config.toml, which is
// worth saying out loud rather than burying. [mcp.servers.<name>]
// already takes a Command and Args, and a skill manifest already takes
// an Install argv — both arbitrary commands from that same file, both
// spawned at boot. Anyone who can edit it can already run code as the
// node, so this extends no trust that has not already been extended. It
// must not WIDEN it, which is what the argv rule above is for.

// pathPlaceholder is replaced in each argv element with the reference's
// path. Present in no element means the path is appended as a final
// argument, which is what `pass show <path>` wants anyway.
const pathPlaceholder = "{{path}}"

// execWaitDelay is how long Run may spend closing pipes after the
// process has been killed. Short: the process is already gone, and this
// only covers descendants still holding the write end.
const execWaitDelay = 2 * time.Second

// subprocessOptionKeys configure the child process rather than the
// vault, so EVERY command-backed driver understands them, including the
// compiled vendor ones.
//
// Kept in one place because they drifted: the vendor factories validated
// only their own keys, so `env_passthrough` was refused at boot on
// exactly the two drivers whose CLIs are most likely to need an unusual
// variable, while the documentation offered it as the migration path.
// `trim_whitespace` had the same gap.
var subprocessOptionKeys = []string{"trim_whitespace", "env_passthrough"}

// execOptionKeys are the options the generic exec driver understands.
var execOptionKeys = subprocessOptionKeys

// ExecFactory builds the generic command-backed provider.
func ExecFactory(cfg ProviderConfig) (Provider, error) {
	if bad := unknownOptions(cfg.Options, execOptionKeys...); len(bad) > 0 {
		return nil, fmt.Errorf("secrets: provider %q: unknown option(s) %v; supported: %s",
			cfg.Label, bad, strings.Join(execOptionKeys, ", "))
	}
	if len(cfg.Command) == 0 || strings.TrimSpace(cfg.Command[0]) == "" {
		return nil, fmt.Errorf(
			`secrets: provider %q: driver = "exec" needs command, e.g. command = ["pass", "show", "%s"]`,
			cfg.Label, pathPlaceholder)
	}
	return newExecProvider(cfg, vendorEnv{})
}

// newExecProvider builds the command-backed provider.
//
// vendor is contributed by the caller, not the operator: a vendor driver
// knows which variables its own CLI authenticates with and adds them, so
// the `export BW_SESSION` and `op signin` workflows its error hints
// recommend keep working without every operator listing them by hand.
//
// Returns an error because env_passthrough is validated here, and a bad
// value should be a boot failure naming the setting rather than a
// provider that silently sees less than the operator configured.
func newExecProvider(cfg ProviderConfig, vendor vendorEnv) (*execProvider, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	trim := true
	if v := option(cfg.Options, "trim_whitespace"); v == "false" {
		// Off for the rare secret whose trailing newline is load
		// bearing. On by default because a CLI that prints a newline is
		// the norm and a key with \n on the end fails authentication in
		// a way nothing reports usefully.
		trim = false
	}
	passthrough, err := parseEnvPassthrough(cfg.Label, option(cfg.Options, "env_passthrough"))
	if err != nil {
		return nil, err
	}
	return &execProvider{
		label: cfg.Label,
		argv:  append([]string(nil), cfg.Command...),
		env:   cfg.Env,
		envAllow: vendorEnv{
			names:    append(append([]string(nil), vendor.names...), passthrough...),
			prefixes: vendor.prefixes,
		},
		timeout: timeout,
		trim:    trim,
	}, nil
}

type execProvider struct {
	label string
	argv  []string
	env   map[string]string
	// envAllow extends baseEnvAllowlist for this provider only: the
	// vendor driver's own credential variables and prefixes, plus
	// whatever the operator named in env_passthrough. Operator names go
	// in names, never prefixes.
	envAllow vendorEnv
	timeout  time.Duration
	trim     bool
}

func (p *execProvider) Fetch(ctx context.Context, path string) (string, error) {
	argv := substitutePath(p.argv, path)

	runCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	// WaitDelay bounds the wait AFTER the context kills the process.
	//
	// Without it the timeout does not hold. CommandContext kills the
	// direct child, but Run waits for the stdout and stderr pipes to
	// close, and any grandchild inherited the write end — so a wrapper
	// script that shells out to something slow keeps the pipes open and
	// Run blocks for as long as the grandchild lives. Measured at 30s
	// against a 150ms timeout before this line existed, which on a
	// boot-time resolve is a node that appears to hang.
	cmd.WaitDelay = execWaitDelay
	cmd.Env = allowedEnv(p.env, p.envAllow)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", p.runError(runCtx, argv, stderr.String(), err)
	}
	out := stdout.String()
	if p.trim {
		out = strings.Trim(out, " \t\r\n")
	}
	if out == "" {
		return "", fmt.Errorf("secrets: provider %q: %s produced no output for %q",
			p.label, argv[0], path)
	}
	return out, nil
}

// cmdError is a failed command, carrying its stderr WHOLE.
//
// The whole copy exists because two jobs read it and they want different
// things. Display wants it short, since it lands in a boot log. Failure
// RECOGNITION — the vendor drivers matching "not signed in" to decide
// which fix to suggest — wants all of it, because the sentence that
// identifies the failure is not reliably in the part that survives
// truncation. Measured: 1Password puts it in the first line of an
// 800-character message and Bitwarden puts it in the last line after
// 180 characters of Node deprecation warnings. Truncating before
// matching would have broken one or the other whichever end was kept.
type cmdError struct {
	label  string
	bin    string
	stderr string
	err    error
}

func (e *cmdError) Error() string {
	if e.stderr != "" {
		return fmt.Sprintf("secrets: provider %q: %s: %s", e.label, e.bin, truncate(e.stderr, stderrDisplayCap))
	}
	return fmt.Sprintf("secrets: provider %q: %s: %v", e.label, e.bin, e.err)
}

func (e *cmdError) Unwrap() error { return e.err }

// runError turns a failed command into something an operator can act
// on. The secret PATH is named because it is not itself a secret and is
// usually the thing that is wrong; the command's stderr is included
// because it is where every CLI puts the real reason. stdout never is —
// that is the secret.
func (p *execProvider) runError(ctx context.Context, argv []string, stderr string, err error) error {
	stderr = strings.TrimSpace(stderr)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("secrets: provider %q: %s timed out after %s; "+
			"a vault CLI that prompts interactively will do this", p.label, argv[0], p.timeout)
	case errors.Is(err, exec.ErrNotFound):
		return fmt.Errorf("secrets: provider %q: %q is not on PATH", p.label, argv[0])
	default:
		return &cmdError{label: p.label, bin: argv[0], stderr: stderr, err: err}
	}
}

// substitutePath replaces the placeholder wherever it appears, and
// appends the path when it appears nowhere.
func substitutePath(argv []string, path string) []string {
	out := make([]string, 0, len(argv)+1)
	found := false
	for _, a := range argv {
		if strings.Contains(a, pathPlaceholder) {
			found = true
			a = strings.ReplaceAll(a, pathPlaceholder, path)
		}
		out = append(out, a)
	}
	if !found {
		out = append(out, path)
	}
	return out
}

// stderrDisplayCap bounds how much of a failed command's stderr reaches
// the log. Generous, because a real one is bigger than it sounds:
// 1Password's "no accounts configured" message is around 800 characters
// of prose and links.
const stderrDisplayCap = 700

// truncate keeps BOTH ENDS of a long stderr.
//
// Neither end is reliably the useful one, which two real CLIs settle
// between them. Bitwarden emits two Node deprecation warnings — about
// 180 characters about the punycode module — and then "You are not
// logged in.", so the tail is what matters. 1Password leads with "No
// accounts configured for use with 1Password CLI." and ends with a
// generic "error initializing client:", so the head is. Keeping one end
// would have thrown away the operative sentence for one of them.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	half := max / 2
	head := textutil.Sanitise(s[:half])
	tail := textutil.Sanitise(s[len(s)-half:])
	return head + "\n…\n" + tail
}

// sortStrings is a local helper so resolver.go does not need the sort
// import for one call.
func sortStrings(s []string) { sort.Strings(s) }
