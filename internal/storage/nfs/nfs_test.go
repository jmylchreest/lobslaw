package nfs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// fakeRunner records every command the mount would execute. Lets
// tests assert on exact argv without touching the real /sbin/mount.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []call
	err     error
	nextOut []byte
}

type call struct {
	name string
	args []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{name: name, args: append([]string(nil), args...)})
	return f.nextOut, f.err
}

func (f *fakeRunner) last() call {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return call{}
	}
	return f.calls[len(f.calls)-1]
}

func TestNewRequiresFields(t *testing.T) {
	t.Parallel()
	cases := []Config{
		{Server: "s", Export: "/e"},
		{Label: "x", Export: "/e"},
		{Label: "x", Server: "s"},
	}
	for i, c := range cases {
		if _, err := NewWithRunner(c, &fakeRunner{}); err == nil {
			t.Errorf("case %d: want err from %+v", i, c)
		}
	}
}

func TestNewRejectsNilRunner(t *testing.T) {
	t.Parallel()
	_, err := NewWithRunner(Config{Label: "x", Server: "s", Export: "/e"}, nil)
	if err == nil {
		t.Error("nil runner should fail")
	}
}

func TestStartInvokesMount(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	m, _ := NewWithRunner(Config{
		Label:     "shared",
		Server:    "nfs.example.com",
		Export:    "/exports/shared",
		MountRoot: t.TempDir(),
	}, r)

	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := r.last()
	if c.name != "mount" {
		t.Errorf("cmd: %q", c.name)
	}
	// argv should contain "-t nfs" and the source:export
	sawType := false
	sawSource := false
	for i, a := range c.args {
		if a == "-t" && i+1 < len(c.args) && c.args[i+1] == "nfs" {
			sawType = true
		}
		if a == "nfs.example.com:/exports/shared" {
			sawSource = true
		}
	}
	if !sawType {
		t.Errorf("missing -t nfs: %v", c.args)
	}
	if !sawSource {
		t.Errorf("missing server:export: %v", c.args)
	}
	if !m.Healthy() {
		t.Error("not healthy after successful Start")
	}
}

func TestStartWithOptionsFlattens(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	m, _ := NewWithRunner(Config{
		Label:     "x",
		Server:    "s",
		Export:    "/e",
		MountRoot: t.TempDir(),
		Options: map[string]string{
			"nfsvers": "4.2",
			"sec":     "krb5p",
			"ro":      "",
		},
	}, r)
	_ = m.Start(context.Background())

	c := r.last()
	var oArg string
	for i, a := range c.args {
		if a == "-o" && i+1 < len(c.args) {
			oArg = c.args[i+1]
		}
	}
	// Keys alphabetised: nfsvers=4.2,ro,sec=krb5p
	want := "nfsvers=4.2,ro,sec=krb5p"
	if oArg != want {
		t.Errorf("options: got %q want %q", oArg, want)
	}
}

func TestStartPropagatesRunnerError(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{err: errors.New("mount: access denied")}
	m, _ := NewWithRunner(Config{
		Label: "x", Server: "s", Export: "/e",
		MountRoot: t.TempDir(),
	}, r)
	err := m.Start(context.Background())
	if err == nil {
		t.Fatal("mount failure should propagate")
	}
}

func TestStopInvokesUmount(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	m, _ := NewWithRunner(Config{
		Label: "x", Server: "s", Export: "/e",
		MountRoot: t.TempDir(),
	}, r)
	_ = m.Start(context.Background())

	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := r.last()
	if c.name != "umount" {
		t.Errorf("expected umount; got %q", c.name)
	}
	if m.Healthy() {
		t.Error("should not be healthy after Stop")
	}
}

func TestStopNoOpIfNotStarted(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	m, _ := NewWithRunner(Config{
		Label: "x", Server: "s", Export: "/e",
		MountRoot: t.TempDir(),
	}, r)
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 0 {
		t.Errorf("Stop-before-Start shouldn't invoke anything; got %+v", r.calls)
	}
}

// TestPathReflectsMountRoot — ensures the computed mountpoint
// includes both MountRoot and Label so multi-label deployments
// don't collide.
func TestPathReflectsMountRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	r := &fakeRunner{}
	m, _ := NewWithRunner(Config{
		Label: "alpha", Server: "s", Export: "/e",
		MountRoot: root,
	}, r)
	_ = m.Start(context.Background())
	want := fmt.Sprintf("%s/%s", root, "alpha")
	if m.Path() != want {
		t.Errorf("Path: got %q want %q", m.Path(), want)
	}
}

// Compile-time interface check.
type mountLike interface {
	Label() string
	Backend() string
	Path() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Healthy() bool
}

func TestNFSMountImplementsInterface(t *testing.T) {
	t.Parallel()
	var _ mountLike = (*Mount)(nil)
}
