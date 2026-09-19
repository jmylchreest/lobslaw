package main

import (
	"os"
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
	t.Parallel()
	original := &sandbox.Policy{
		NoNewPrivs:   true,
		AllowedPaths: []string{"/tmp/work"},
		Seccomp:      sandbox.SeccompPolicy{Deny: []string{"ptrace"}},
	}
	encoded, err := sandbox.EncodePolicy(original)
	if err != nil {
		t.Fatal(err)
	}
	if encoded == "" {
		t.Fatal("encode returned empty string for non-nil policy")
	}
	got, err := sandbox.DecodePolicy(encoded)
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

func TestDecodePolicyEmptyReturnsZeroPolicy(t *testing.T) {
	t.Parallel()
	p, err := sandbox.DecodePolicy("")
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		t.Fatal("should return zero Policy, not nil")
	}
	if p.NoNewPrivs || len(p.AllowedPaths) > 0 {
		t.Errorf("empty input should yield zero Policy; got %+v", *p)
	}
}

func TestDecodePolicyMalformedBase64Errors(t *testing.T) {
	t.Parallel()
	if _, err := sandbox.DecodePolicy("!!!not-base64!!!"); err == nil {
		t.Error("malformed base64 should surface an error")
	}
}

func TestDecodePolicyBase64ButNotJSONErrors(t *testing.T) {
	t.Parallel()
	if _, err := sandbox.DecodePolicy("aGVsbG8gd29ybGQ="); err == nil || !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("non-JSON base64 should fail with unmarshal error, got %v", err)
	}
}

// An absent policy must fail before the helper can replace this process.
func TestRunSandboxExecRequiresPolicy(t *testing.T) {
	for _, unset := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "unset"}[unset], func(t *testing.T) {
			t.Setenv(sandbox.PolicyEnvVar, "")
			if unset {
				if err := os.Unsetenv(sandbox.PolicyEnvVar); err != nil {
					t.Fatal(err)
				}
			}
			err := runSandboxExec([]string{"--", "/bin/true"})
			if err == nil || !strings.Contains(err.Error(), sandbox.PolicyEnvVar+" is required") {
				t.Fatalf("expected missing-policy error, got %v", err)
			}
		})
	}
}

func TestRunSandboxExecDecodesSuppliedPolicy(t *testing.T) {
	raw, err := sandbox.EncodePolicy(&sandbox.Policy{NoNewPrivs: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(sandbox.PolicyEnvVar, raw)
	// Invalid target stops before exec, after decoding and scrubbing the policy.
	err = runSandboxExec([]string{"relative"})
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("expected target validation, got %v", err)
	}
	if _, ok := os.LookupEnv(sandbox.PolicyEnvVar); ok {
		t.Fatal("policy was not scrubbed")
	}
}
