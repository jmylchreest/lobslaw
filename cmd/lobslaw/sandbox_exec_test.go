package main

import (
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
)

func TestParseTargetInvocationStripsDoubleDash(t *testing.T) {
	t.Parallel()
	target, argv, err := parseTargetInvocation([]string{"--", "/bin/echo", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if target != "/bin/echo" {
		t.Errorf("target: got %q, want /bin/echo", target)
	}
	if len(argv) != 2 || argv[0] != "/bin/echo" || argv[1] != "hi" {
		t.Errorf("argv: got %v, want [/bin/echo hi]", argv)
	}
}

func TestParseTargetInvocationWithoutDoubleDash(t *testing.T) {
	t.Parallel()
	target, argv, err := parseTargetInvocation([]string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	if target != "/bin/true" || len(argv) != 1 || argv[0] != "/bin/true" {
		t.Errorf("got target=%q argv=%v", target, argv)
	}
}

func TestParseTargetInvocationRejectsEmpty(t *testing.T) {
	t.Parallel()
	if _, _, err := parseTargetInvocation(nil); err == nil {
		t.Error("empty args should be rejected")
	}
	if _, _, err := parseTargetInvocation([]string{"--"}); err == nil {
		t.Error("`--` alone should be rejected")
	}
}

func TestParseTargetInvocationRejectsRelativePath(t *testing.T) {
	t.Parallel()
	if _, _, err := parseTargetInvocation([]string{"--", "bin/echo"}); err == nil {
		t.Error("SECURITY: relative target path should be rejected")
	}
}

func TestEncodeDecodePolicyRoundTrip(t *testing.T) {
	original := &sandbox.Policy{
		NoNewPrivs:   true,
		AllowedPaths: []string{"/tmp/work"},
		Seccomp:      sandbox.SeccompPolicy{Deny: []string{"ptrace"}},
	}
	encoded, err := encodeSandboxPolicy(original)
	if err != nil {
		t.Fatal(err)
	}
	if encoded == "" {
		t.Fatal("encode returned empty string for non-nil policy")
	}

	t.Setenv(envSandboxPolicy, encoded)
	got, err := readSandboxPolicyFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !got.NoNewPrivs {
		t.Error("NoNewPrivs didn't survive round-trip")
	}
	if len(got.AllowedPaths) != 1 || got.AllowedPaths[0] != "/tmp/work" {
		t.Errorf("AllowedPaths didn't survive: %v", got.AllowedPaths)
	}
	if len(got.Seccomp.Deny) != 1 || got.Seccomp.Deny[0] != "ptrace" {
		t.Errorf("Seccomp didn't survive: %v", got.Seccomp.Deny)
	}
}

func TestReadPolicyEmptyEnvReturnsZeroPolicy(t *testing.T) {
	t.Parallel()
	p, err := readSandboxPolicyFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		t.Fatal("should return zero Policy, not nil")
	}
	if p.NoNewPrivs || len(p.AllowedPaths) > 0 {
		t.Errorf("empty env should yield zero Policy; got %+v", *p)
	}
}

func TestReadPolicyMalformedEnvErrors(t *testing.T) {
	t.Setenv(envSandboxPolicy, "!!!not-base64!!!")
	if _, err := readSandboxPolicyFromEnv(); err == nil {
		t.Error("malformed base64 should surface an error")
	}
}

func TestReadPolicyBase64ButNotJSONErrors(t *testing.T) {
	t.Setenv(envSandboxPolicy, "aGVsbG8gd29ybGQ=") // "hello world"
	if _, err := readSandboxPolicyFromEnv(); err == nil || !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("non-JSON base64 should fail with unmarshal error, got %v", err)
	}
}
