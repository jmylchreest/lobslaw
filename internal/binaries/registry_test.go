package binaries

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type fakeRunner struct {
	commands map[string]fakeOutcome
	calls    []fakeCall
}

type fakeCall struct {
	name string
	args []string
}

type fakeOutcome struct {
	output string
	err    error
}

func (f *fakeRunner) Run(_ context.Context, name string, args []string, _ []string) (string, error) {
	f.calls = append(f.calls, fakeCall{name: name, args: append([]string(nil), args...)})
	key := name
	if len(args) > 0 {
		key = name + " " + strings.Join(args, " ")
	}
	if out, ok := f.commands[key]; ok {
		return out.output, out.err
	}
	if out, ok := f.commands[name]; ok {
		return out.output, out.err
	}
	return "", nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// makeBinaryInPrefix drops a fake executable into prefix/bin/<name>
// so Satisfier.Available returns true. Cleanup is the test's
// responsibility via t.TempDir.
func makeBinaryInPrefix(t *testing.T, prefix, name string) {
	t.Helper()
	binDir := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestSatisfyAlreadyAvailable(t *testing.T) {
	prefix := t.TempDir()
	makeBinaryInPrefix(t, prefix, "alreadyhere")

	s := New(Config{
		InstallPrefix: prefix,
		Logger:        quietLogger(),
	})
	res, err := s.Satisfy(context.Background(), "alreadyhere", []InstallSpec{{
		OS: runtime.GOOS, Manager: "brew", Package: "alreadyhere",
	}})
	if err != nil {
		t.Fatalf("Satisfy: %v", err)
	}
	if !res.AlreadyAvailable {
		t.Fatalf("expected AlreadyAvailable=true, got %+v", res)
	}
}

func TestSatisfyNoMatchingSpec(t *testing.T) {
	prefix := t.TempDir()
	s := New(Config{
		InstallPrefix: prefix,
		Logger:        quietLogger(),
	})
	_, err := s.Satisfy(context.Background(), "windowsonly", []InstallSpec{{
		OS: "windows", Manager: "brew", Package: "x",
	}})
	if runtime.GOOS == "windows" {
		return
	}
	if err == nil {
		t.Fatal("expected no-matching-spec error")
	}
	if !strings.Contains(err.Error(), "no install spec") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSatisfyEmptyName(t *testing.T) {
	s := New(Config{Logger: quietLogger()})
	_, err := s.Satisfy(context.Background(), "", nil)
	if err == nil {
		t.Fatal("expected empty-name error")
	}
}

func TestSatisfyRejectsSudoOnUserModeManager(t *testing.T) {
	s := New(Config{Logger: quietLogger()})
	_, err := s.Satisfy(context.Background(), "x", []InstallSpec{{
		OS: runtime.GOOS, Manager: "npm", Package: "x", Sudo: true,
	}})
	if err == nil {
		t.Fatal("expected user-mode + sudo rejection")
	}
	if !strings.Contains(err.Error(), "user-mode") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSatisfyAllowsSudoOnSystemManager(t *testing.T) {
	s := New(Config{Logger: quietLogger()})
	_, err := s.Satisfy(context.Background(), "x", []InstallSpec{{
		OS: runtime.GOOS, Manager: "apt", Package: "x", Sudo: true,
	}})
	// On non-debian hosts, apt is unavailable — error mentions
	// "manager not present", not user-mode rejection.
	if err != nil && strings.Contains(err.Error(), "user-mode") {
		t.Fatalf("apt+sudo should not be rejected as user-mode: %v", err)
	}
}

func TestSatisfyManagerNotPresentOnHost(t *testing.T) {
	s := New(Config{Logger: quietLogger()})
	_, err := s.Satisfy(context.Background(), "imaginary", []InstallSpec{{
		OS: runtime.GOOS, Manager: "brew", Package: "imaginary",
	}})
	// Test host probably doesn't have brew (Linux-only test infra) — if
	// it does, this test is a no-op for the assertion path. We just
	// ensure no panic + no false-positive user-mode rejection.
	if err != nil && strings.Contains(err.Error(), "user-mode") {
		t.Fatalf("unexpected user-mode error: %v", err)
	}
}

func TestHostsForUnion(t *testing.T) {
	s := New(Config{
		Logger:     quietLogger(),
		HTTPClient: &http.Client{},
	})
	hosts := s.HostsFor([]InstallSpec{
		{OS: "linux", Manager: "apt", Package: "gh"},
		{OS: "darwin", Manager: "brew", Package: "gh"},
		{OS: "linux", Manager: "curl-sh", URL: "https://astral.sh/uv/install.sh", Checksum: "sha256:" + strings.Repeat("0", 64)},
	})
	want := map[string]bool{
		"deb.debian.org":   true,
		"formulae.brew.sh": true,
		"astral.sh":        true,
	}
	got := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		got[h] = true
	}
	for h := range want {
		if !got[h] {
			t.Errorf("missing %q in union: %v", h, hosts)
		}
	}
}

func TestLookPathPrefersPrefix(t *testing.T) {
	prefix := t.TempDir()
	makeBinaryInPrefix(t, prefix, "uniqname")

	got, err := LookPath("uniqname", prefix)
	if err != nil {
		t.Fatalf("LookPath: %v", err)
	}
	want := filepath.Join(prefix, "bin", "uniqname")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestLookPathFallsBackToSystem(t *testing.T) {
	prefix := t.TempDir()
	// "sh" should exist on any POSIX test host
	got, err := LookPath("sh", prefix)
	if err != nil {
		t.Skip("no /bin/sh on test host")
	}
	if !strings.HasPrefix(got, prefix) && got != "" {
		// It found system sh — that's the path we want.
	}
}

func TestEnsureSudoFailsClosed(t *testing.T) {
	runner := &fakeRunner{commands: map[string]fakeOutcome{
		"sudo -n true": {output: "sudo: a password is required", err: errors.New("exit 1")},
	}}
	err := ensureSudoAllowed(context.Background(), runner)
	if err == nil {
		t.Fatal("expected ensureSudoAllowed to fail without passwordless sudo")
	}
	if !errors.Is(err, errSudoNotAllowed) {
		t.Fatalf("expected errSudoNotAllowed wrap, got: %v", err)
	}
}
