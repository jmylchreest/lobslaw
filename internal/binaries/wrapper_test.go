package binaries

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureEnvWrapper(t *testing.T) {
	prefix := t.TempDir()
	binDir := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	binPath := filepath.Join(binDir, "fakecli")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho real\n"), 0o755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}

	status, err := EnsureEnvWrapper(prefix, "fakecli", []string{"HOME=/tmp/xdg", "FOO=bar"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if status != WrapperApplied {
		t.Fatalf("first apply status = %v, want applied", status)
	}

	libexec := filepath.Join(prefix, "libexec", "fakecli")
	if _, err := os.Stat(libexec); err != nil {
		t.Fatalf("libexec/fakecli should exist after apply: %v", err)
	}
	body, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read shim: %v", err)
	}
	s := string(body)
	if !strings.HasPrefix(s, "#!/bin/sh\n# lobslaw-managed env wrapper") {
		t.Fatalf("shim missing header: %q", s)
	}
	if !strings.Contains(s, `export FOO='bar'`) {
		t.Fatalf("shim missing FOO export: %q", s)
	}
	if !strings.Contains(s, `export HOME='/tmp/xdg'`) {
		t.Fatalf("shim missing HOME export: %q", s)
	}
	if !strings.Contains(s, `exec "$(dirname "$0")/../libexec/fakecli"`) {
		t.Fatalf("shim missing exec line: %q", s)
	}

	status2, err := EnsureEnvWrapper(prefix, "fakecli", []string{"FOO=bar", "HOME=/tmp/xdg"})
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if status2 != WrapperUnchanged {
		t.Fatalf("re-apply status = %v, want unchanged", status2)
	}

	status3, err := EnsureEnvWrapper(prefix, "fakecli", []string{"HOME=/tmp/xdg", "FOO=baz"})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if status3 != WrapperApplied {
		t.Fatalf("refresh status = %v, want applied", status3)
	}
	body, _ = os.ReadFile(binPath)
	if !strings.Contains(string(body), `export FOO='baz'`) {
		t.Fatalf("refresh did not update FOO: %q", string(body))
	}

	status4, err := EnsureEnvWrapper(prefix, "fakecli", nil)
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if status4 != WrapperRemoved {
		t.Fatalf("teardown status = %v, want removed", status4)
	}
	if _, err := os.Stat(libexec); !os.IsNotExist(err) {
		t.Fatalf("libexec should be gone after teardown, err=%v", err)
	}
	body, _ = os.ReadFile(binPath)
	if !strings.HasPrefix(string(body), "#!/bin/sh\necho real") {
		t.Fatalf("real binary not restored: %q", string(body))
	}
}

func TestEnsureEnvWrapperNotApplicable(t *testing.T) {
	prefix := t.TempDir()
	status, err := EnsureEnvWrapper(prefix, "ghost", []string{"HOME=/x"})
	if err != nil {
		t.Fatalf("not-applicable should not error: %v", err)
	}
	if status != WrapperNotApplicable {
		t.Fatalf("status = %v, want not-applicable", status)
	}
}

func TestEnsureEnvWrapperShellEscape(t *testing.T) {
	prefix := t.TempDir()
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(prefix, "bin", "x")
	if err := os.WriteFile(binPath, []byte("real"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := EnsureEnvWrapper(prefix, "x", []string{"WEIRD=it's ok"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	body, _ := os.ReadFile(binPath)
	if !strings.Contains(string(body), `export WEIRD='it'\''s ok'`) {
		t.Fatalf("single-quote escape failed: %q", string(body))
	}
}
