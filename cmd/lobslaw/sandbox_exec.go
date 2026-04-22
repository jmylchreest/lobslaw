package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
)

// envSandboxPolicy is the env var the parent process uses to ferry
// the serialised Policy into the sandbox-exec helper child. Chosen
// (rather than argv) so the policy doesn't appear in `ps`, and so
// argv stays purely the "-- /real/tool args..." tail the user would
// expect to see.
const envSandboxPolicy = "LOBSLAW_SANDBOX_POLICY"

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
	if len(args) < 1 || args[0] != "sandbox-exec" {
		return false
	}
	if err := runSandboxExec(args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lobslaw sandbox-exec:", err)
		os.Exit(1)
	}
	// InstallAndExec doesn't return on success; if we reach here, the
	// exec returned without an error which is not possible — fail loud.
	fmt.Fprintln(os.Stderr, "lobslaw sandbox-exec: exec returned without error (unreachable)")
	os.Exit(1)
	return true
}

// runSandboxExec parses the subcommand's argument tail and invokes
// sandbox.InstallAndExec. Kept separate from dispatch so tests can
// exercise the arg parsing without os.Exit.
func runSandboxExec(args []string) error {
	policy, err := readSandboxPolicyFromEnv()
	if err != nil {
		return err
	}
	// Scrub the env var so the target tool doesn't inherit it — it's
	// meta-information about the sandbox wrapper, not something the
	// tool should see.
	if err := os.Unsetenv(envSandboxPolicy); err != nil {
		return fmt.Errorf("unset %s: %w", envSandboxPolicy, err)
	}

	target, argv, err := parseTargetInvocation(args)
	if err != nil {
		return err
	}

	// Inherit the remaining environment. The parent (Executor) already
	// whitelisted it via ExecutorConfig.EnvWhitelist.
	env := os.Environ()
	return sandbox.InstallAndExec(policy, target, argv, env)
}

// readSandboxPolicyFromEnv decodes the base64-JSON policy carried in
// $LOBSLAW_SANDBOX_POLICY. An empty/unset env is treated as a zero
// Policy (caller wanted the reexec shape without any enforcement —
// useful for testing the dispatch path).
func readSandboxPolicyFromEnv() (*sandbox.Policy, error) {
	raw := os.Getenv(envSandboxPolicy)
	if raw == "" {
		return &sandbox.Policy{}, nil
	}
	blob, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", envSandboxPolicy, err)
	}
	var p sandbox.Policy
	if err := json.Unmarshal(blob, &p); err != nil {
		return nil, fmt.Errorf("unmarshal sandbox policy: %w", err)
	}
	return &p, nil
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
	// argv[0] is conventionally the path too — tools expect this.
	argv := append([]string{target}, args[1:]...)
	return target, argv, nil
}

// encodeSandboxPolicy serialises a Policy for transport via
// $LOBSLAW_SANDBOX_POLICY. Used by the parent (Apply) and mirrored
// in tests.
func encodeSandboxPolicy(p *sandbox.Policy) (string, error) {
	if p == nil {
		return "", nil
	}
	blob, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(blob), nil
}
