package main

import (
	"fmt"
	"os"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
)

// dispatchSandboxExec recognises the hidden `lobslaw sandbox-exec`
// subcommand and, when matched, hands off to runSandboxExec which
// does NOT return on success (exec-replaces the process). Returns
// true if the subcommand ran (caller should exit without further
// work), false if the args didn't match.
//
// Invocation shape:
//
//	lobslaw sandbox-exec -- /real/tool arg1 arg2 ...
//
// Policy is read from $LOBSLAW_SANDBOX_POLICY (base64-encoded JSON)
// to keep argv clean and avoid ps visibility.
func dispatchSandboxExec(args []string) bool {
	if len(args) < 1 || args[0] != sandbox.HelperSubcommand {
		return false
	}
	if err := runSandboxExec(args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lobslaw sandbox-exec:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "lobslaw sandbox-exec: exec returned without error (unreachable)")
	os.Exit(1)
	return true
}

// runSandboxExec parses the subcommand's argument tail and invokes
// sandbox.InstallAndExec. Kept separate from dispatch so tests can
// exercise the arg parsing without os.Exit.
func runSandboxExec(args []string) error {
	policy, err := sandbox.DecodePolicy(os.Getenv(sandbox.PolicyEnvVar))
	if err != nil {
		return err
	}
	// Scrub the env var so the target tool doesn't inherit it — it's
	// meta-information about the sandbox wrapper, not something the
	// tool should see.
	if err := os.Unsetenv(sandbox.PolicyEnvVar); err != nil {
		return fmt.Errorf("unset %s: %w", sandbox.PolicyEnvVar, err)
	}

	target, argv, err := parseTargetInvocation(args)
	if err != nil {
		return err
	}
	env := os.Environ()
	return sandbox.InstallAndExec(policy, target, argv, env)
}

// parseTargetInvocation splits `["--", "/bin/tool", "arg1", "arg2"]`
// into (target="/bin/tool", argv=["/bin/tool", "arg1", "arg2"]). The
// leading `--` is optional (to tolerate callers that pass it as a
// separator) but the first non-`--` token is required and must be an
// absolute path.
func parseTargetInvocation(args []string) (string, []string, error) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return "", nil, fmt.Errorf("no target specified after `--`")
	}
	target := args[0]
	if len(target) == 0 || target[0] != '/' {
		return "", nil, fmt.Errorf("target path %q must be absolute", target)
	}
	argv := append([]string{target}, args[1:]...)
	return target, argv, nil
}
