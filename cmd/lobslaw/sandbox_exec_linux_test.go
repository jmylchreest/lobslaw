//go:build linux

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	encoded, err := encodeSandboxPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "sandbox-exec", "--", "/bin/cat", "/proc/self/status")
	cmd.Env = append(os.Environ(), envSandboxPolicy+"="+encoded)
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

	cmd := exec.Command(bin, "sandbox-exec", "--", "/bin/cat", "/proc/self/status")
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

// TestSandboxExecRejectsBadTarget verifies the helper surfaces a
// clean error (not a crash / exec of something unexpected) when the
// target isn't absolute. Runs the same binary but expects non-zero
// exit + a specific stderr substring.
func TestSandboxExecRejectsBadTarget(t *testing.T) {
	bin := buildHelperBinary(t)

	cmd := exec.Command(bin, "sandbox-exec", "--", "relative/path")
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
