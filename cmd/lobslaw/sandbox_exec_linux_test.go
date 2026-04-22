//go:build linux

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
)

// buildHelperBinary builds the lobslaw binary into a tempdir and
// returns its path. Used by integration tests that need to exec the
// real sandbox-exec subcommand (plain `go test` doesn't give us the
// compiled binary otherwise).
func buildHelperBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "lobslaw")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build lobslaw: %v", err)
	}
	return bin
}

// TestSandboxExecNoNewPrivsSetsProcStatus spawns the helper binary
// with a NoNewPrivs-only policy and asserts the target process sees
// NoNewPrivs=1 in /proc/self/status. End-to-end proof that the
// reexec pathway actually installs the prctl flag before execve.
func TestSandboxExecNoNewPrivsSetsProcStatus(t *testing.T) {
	bin := buildHelperBinary(t)

	policy := &sandbox.Policy{NoNewPrivs: true}
	encoded, err := sandbox.EncodePolicy(policy)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, sandbox.HelperSubcommand, "--", "/bin/cat", "/proc/self/status")
	cmd.Env = append(os.Environ(), sandbox.PolicyEnvVar+"="+encoded)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		t.Fatalf("helper run failed: %v\n--- output ---\n%s", err, out.String())
	}

	if !strings.Contains(out.String(), "\nNoNewPrivs:\t1\n") {
		t.Errorf("expected NoNewPrivs=1 in /proc/self/status; got:\n%s", out.String())
	}
}

// TestSandboxExecWithoutNoNewPrivsLeavesProcStatusClear confirms we
// only flip the bit when the policy asks — zero-value Policy is
// "dispatch-only" (tests the reexec wiring without any enforcement).
func TestSandboxExecWithoutNoNewPrivsLeavesProcStatusClear(t *testing.T) {
	bin := buildHelperBinary(t)

	cmd := exec.Command(bin, sandbox.HelperSubcommand, "--", "/bin/cat", "/proc/self/status")
	// No LOBSLAW_SANDBOX_POLICY env — helper defaults to zero Policy.
	cmd.Env = os.Environ()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		t.Fatalf("helper run failed: %v\n--- output ---\n%s", err, out.String())
	}

	// A process that did NOT flip NoNewPrivs inherits from the parent.
	// We run via `go test`, which itself probably has NoNewPrivs=0,
	// so we expect the 0 to show through here. This guards against a
	// future regression where we accidentally default the prctl on.
	if !strings.Contains(out.String(), "\nNoNewPrivs:\t0\n") {
		t.Errorf("expected NoNewPrivs=0 when policy is zero; got:\n%s", out.String())
	}
}

// TestSandboxExecLandlockBlocksOutsideAllowedPaths proves the
// Landlock install path actually restricts filesystem access:
//
//   - A file INSIDE AllowedPaths reads successfully (sandbox doesn't
//     blanket-deny; otherwise the test is vacuous).
//   - A file OUTSIDE AllowedPaths returns Permission denied at the
//     kernel level — no userspace check could produce this message
//     against /etc/passwd without Landlock intervening.
//
// The host needs Linux 5.13+ with the Landlock LSM enabled; we skip
// the test on older kernels since BestEffort would silently no-op.
func TestSandboxExecLandlockBlocksOutsideAllowedPaths(t *testing.T) {
	if !landlockSupported(t) {
		t.Skip("kernel doesn't expose Landlock LSM")
	}
	bin := buildHelperBinary(t)

	workDir := t.TempDir()
	insideFile := filepath.Join(workDir, "allowed.txt")
	if err := os.WriteFile(insideFile, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	policy := &sandbox.Policy{
		NoNewPrivs: true,
		// /usr covers cat binary + libs on merged-/usr distros (most
		// modern Linux). ReadOnlyPaths keeps system dirs immutable to
		// the tool; workDir stays RW.
		AllowedPaths:  []string{"/usr", workDir},
		ReadOnlyPaths: []string{"/usr"},
	}
	encoded, err := sandbox.EncodePolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	envPolicy := sandbox.PolicyEnvVar + "=" + encoded

	t.Run("inside_allowed_reads", func(t *testing.T) {
		cmd := exec.Command(bin, sandbox.HelperSubcommand, "--", "/usr/bin/cat", insideFile)
		cmd.Env = append(os.Environ(), envPolicy)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("cat on allowed file failed: %v\n%s", err, out.String())
		}
		if out.String() != "ok" {
			t.Errorf("expected %q, got %q", "ok", out.String())
		}
	})

	t.Run("outside_denied_with_eacces", func(t *testing.T) {
		cmd := exec.Command(bin, sandbox.HelperSubcommand, "--", "/usr/bin/cat", "/etc/passwd")
		cmd.Env = append(os.Environ(), envPolicy)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		if err == nil {
			t.Fatalf("SECURITY: cat /etc/passwd succeeded with Landlock active:\n%s", out.String())
		}
		if !strings.Contains(strings.ToLower(out.String()), "permission denied") {
			t.Errorf("expected 'permission denied', got: %s", out.String())
		}
	})
}

// landlockSupported probes the kernel for Landlock by calling
// landlock_create_ruleset(NULL, 0, LANDLOCK_CREATE_RULESET_VERSION).
// A success return gives the ABI version (>= 1); ENOSYS / EOPNOTSUPP
// means unavailable. Runs via a subprocess so the probe doesn't
// itself get sandboxed.
func landlockSupported(t *testing.T) bool {
	t.Helper()
	// syscall 444 = landlock_create_ruleset; arg3 == 1 means "return
	// the ABI version". -errno on failure.
	const sysLandlockCreateRuleset = 444
	const landlockCreateRulesetVersion = 1
	r1, _, errno := syscall.Syscall(sysLandlockCreateRuleset, 0, 0, landlockCreateRulesetVersion)
	if errno != 0 {
		return false
	}
	return int(r1) >= 1
}

// TestSandboxExecSeccompBlocksDeniedSyscall proves the seccomp BPF
// filter is loaded and blocks syscalls in Policy.Seccomp.Deny at
// the kernel level with EPERM. Uses /usr/bin/unshare because:
//
//   - it invokes the `unshare` syscall directly (one of the denies
//     in DefaultSeccompPolicy — the test would fail if we ever
//     removed it from the deny-list without updating both places)
//   - failure is a clean "Operation not permitted" (EPERM) that's
//     hard to produce by accident from any other cause
//
// Control: same invocation without seccomp succeeds — confirms the
// test isn't passing because of some unrelated restriction.
func TestSandboxExecSeccompBlocksDeniedSyscall(t *testing.T) {
	if _, err := os.Stat("/usr/bin/unshare"); err != nil {
		t.Skipf("/usr/bin/unshare not available: %v", err)
	}
	bin := buildHelperBinary(t)

	t.Run("control_no_seccomp_succeeds", func(t *testing.T) {
		// Baseline: unshare --user succeeds unprivileged on modern
		// Linux. If this fails, the system has unprivileged_userns_clone
		// disabled and our seccomp test below is vacuous — skip.
		cmd := exec.Command(bin, sandbox.HelperSubcommand, "--", "/usr/bin/unshare", "--user", "/bin/true")
		cmd.Env = os.Environ()
		if err := cmd.Run(); err != nil {
			t.Skipf("baseline unshare failed (system disables unprivileged userns?): %v", err)
		}
	})

	t.Run("with_seccomp_unshare_denied", func(t *testing.T) {
		policy := &sandbox.Policy{
			NoNewPrivs: true,
			Seccomp:    sandbox.DefaultSeccompPolicy,
		}
		encoded, err := sandbox.EncodePolicy(policy)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, sandbox.HelperSubcommand, "--", "/usr/bin/unshare", "--user", "/bin/true")
		cmd.Env = append(os.Environ(), sandbox.PolicyEnvVar+"="+encoded)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err = cmd.Run()
		if err == nil {
			t.Fatalf("SECURITY: unshare syscall succeeded with seccomp active:\n%s", out.String())
		}
		if !strings.Contains(strings.ToLower(out.String()), "operation not permitted") {
			t.Errorf("expected EPERM from seccomp, got: %s", out.String())
		}
	})
}

// TestApplyEndToEndRewritesForHelperAndEnforces proves the full
// parent-side integration: sandbox.Apply rewrites a cmd to reexec
// through the helper, policy traverses the env var, and the target
// binary actually sees enforced NoNewPrivs=1 in /proc/self/status.
//
// Distinct from the earlier unit tests on parseTargetInvocation:
// those exercised the helper child in isolation, this one runs the
// parent pipeline from sandbox.Apply(cmd, policy) end-to-end.
func TestApplyEndToEndRewritesForHelperAndEnforces(t *testing.T) {
	bin := buildHelperBinary(t)

	policy := &sandbox.Policy{NoNewPrivs: true}

	cmd := exec.Command("/bin/cat", "/proc/self/status")
	cmd.Env = os.Environ()
	if err := sandbox.Apply(cmd, policy); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Apply rewrote cmd to use /proc/self/exe — but /proc/self/exe
	// within `go test` resolves to the test binary, not lobslaw. Point
	// cmd.Path at our freshly-built lobslaw binary so the reexec
	// dispatches to dispatchSandboxExec.
	cmd.Path = bin

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("rewritten cmd failed: %v\n--- output ---\n%s", err, out.String())
	}

	if !strings.Contains(out.String(), "\nNoNewPrivs:\t1\n") {
		t.Errorf("expected NoNewPrivs=1 after Apply+reexec; got:\n%s", out.String())
	}
	// Proves the original argv reached the target: cat received its
	// file arg and actually read /proc/self/status (rather than the
	// helper eating the arg or the reexec clobbering argv).
	if !strings.Contains(out.String(), "Name:\t") {
		t.Errorf("expected /proc/self/status content (Name: line) in output:\n%s", out.String())
	}
}

// TestApplyNoEnforcementFieldsDoesNotRewrite confirms policies that
// only ask for namespaces/resource-limits don't trigger the reexec
// helper — those layers apply via SysProcAttr directly, and going
// through the helper would (a) add spawn latency and (b) break the
// Apply contract for callers that explicitly don't want enforcement.
func TestApplyNoEnforcementFieldsDoesNotRewrite(t *testing.T) {
	t.Parallel()
	policy := &sandbox.Policy{
		Namespaces: sandbox.NamespaceSet{Mount: true},
	}
	cmd := exec.Command("/bin/true")
	originalPath := cmd.Path
	if err := sandbox.Apply(cmd, policy); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if cmd.Path != originalPath {
		t.Errorf("namespaces-only policy shouldn't rewrite cmd.Path; got %q", cmd.Path)
	}
}

// TestSandboxExecRejectsBadTarget verifies the helper surfaces a
// clean error (not a crash / exec of something unexpected) when the
// target isn't absolute. Runs the same binary but expects non-zero
// exit + a specific stderr substring.
func TestSandboxExecRejectsBadTarget(t *testing.T) {
	bin := buildHelperBinary(t)

	cmd := exec.Command(bin, sandbox.HelperSubcommand, "--", "relative/path")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatal("helper with relative target should exit non-zero")
	}
	if !strings.Contains(stderr.String(), "must be absolute") {
		t.Errorf("expected 'must be absolute' in stderr, got: %s", stderr.String())
	}
}
