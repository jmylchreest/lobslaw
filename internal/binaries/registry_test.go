package binaries

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"testing"
)

// fakeRunner is the deterministic ProcessRunner test stub. Each call
// is recorded; lookups in commands map decide the exit / output.
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

func TestRegistryNewRejectsDuplicateName(t *testing.T) {
	_, err := New(Config{
		Binaries: []Binary{
			{Name: "gh", Install: []InstallSpec{{OS: runtime.GOOS, Manager: "apt", Package: "gh"}}},
			{Name: "gh", Install: []InstallSpec{{OS: runtime.GOOS, Manager: "brew", Package: "gh"}}},
		},
		Logger: quietLogger(),
	})
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Fatalf("expected duplicate-name error, got: %v", err)
	}
}

func TestRegistryListReportsInstallState(t *testing.T) {
	runner := &fakeRunner{commands: map[string]fakeOutcome{
		"true": {},
	}}
	reg, err := New(Config{
		Binaries: []Binary{{
			Name:    "ghx",
			Detect:  "true",
			Install: []InstallSpec{{OS: runtime.GOOS, Manager: "apt", Package: "ghx"}},
		}},
		Runner: runner,
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	entries := reg.List(context.Background())
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if !entries[0].Installed {
		t.Fatalf("expected detect=true to mark installed, got: %+v", entries[0])
	}
	if entries[0].HostSupport != "supported" {
		t.Fatalf("expected supported host, got %q", entries[0].HostSupport)
	}
}

func TestRegistryInstallShortCircuitsOnDetect(t *testing.T) {
	runner := &fakeRunner{commands: map[string]fakeOutcome{
		"true": {}, // detect succeeds → already installed
	}}
	reg, err := New(Config{
		Binaries: []Binary{{
			Name:    "alreadyhere",
			Detect:  "true",
			Install: []InstallSpec{{OS: runtime.GOOS, Manager: "brew", Package: "alreadyhere"}},
		}},
		Runner: runner,
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := reg.Install(context.Background(), "alreadyhere")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !result.AlreadyInstalled {
		t.Fatalf("expected AlreadyInstalled=true, got %+v", result)
	}
	for _, c := range runner.calls {
		if c.name == "brew" || (c.name == "sudo" && len(c.args) > 0 && c.args[0] == "-n") {
			t.Fatalf("install ran the manager despite detect succeeding: %+v", c)
		}
	}
}

func TestRegistryInstallUnknownBinary(t *testing.T) {
	reg, err := New(Config{Logger: quietLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = reg.Install(context.Background(), "nope")
	if err == nil {
		t.Fatal("expected unknown-binary error")
	}
	if !strings.Contains(err.Error(), "not in catalogue") {
		t.Fatalf("expected catalogue error, got: %v", err)
	}
}

func TestRegistryInstallNoMatchingSpec(t *testing.T) {
	reg, err := New(Config{
		Binaries: []Binary{{
			Name:    "windowsonly",
			Install: []InstallSpec{{OS: "windows", Manager: "brew", Package: "x"}},
		}},
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = reg.Install(context.Background(), "windowsonly")
	if runtime.GOOS == "windows" {
		// brew probably isn't there but the spec matched OS.
		// Either no manager available OR empty error path is fine.
		return
	}
	if err == nil {
		t.Fatal("expected no-spec error on non-windows host")
	}
	if !strings.Contains(err.Error(), "no install spec") {
		t.Fatalf("expected no-spec error, got: %v", err)
	}
}

func TestHostsFromBinariesUnion(t *testing.T) {
	specs := []Binary{
		{
			Name: "gh",
			Install: []InstallSpec{
				{OS: "linux", Manager: "apt", Package: "gh"},
				{OS: "darwin", Manager: "brew", Package: "gh"},
			},
		},
		{
			Name: "uvx",
			Install: []InstallSpec{
				{OS: "linux", Manager: "curl-sh", URL: "https://astral.sh/uv/install.sh", Checksum: "sha256:" + strings.Repeat("0", 64)},
			},
		},
	}
	hosts := HostsFromBinaries(specs)
	want := map[string]bool{
		"deb.debian.org":          true,
		"formulae.brew.sh":        true,
		"github.com":              true,
		"astral.sh":               true,
	}
	gotSet := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		gotSet[h] = true
	}
	for h := range want {
		if !gotSet[h] {
			t.Errorf("missing host %q in union: %v", h, hosts)
		}
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
