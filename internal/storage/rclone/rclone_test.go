package rclone

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type call struct {
	name string
	env  []string
	args []string
}

type fakeRunner struct {
	mu    sync.Mutex
	calls []call
	err   error
}

func (f *fakeRunner) Run(_ context.Context, name string, env []string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{name: name, env: append([]string(nil), env...), args: append([]string(nil), args...)})
	return nil, f.err
}

func (f *fakeRunner) last() call {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return call{}
	}
	return f.calls[len(f.calls)-1]
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	_, err := NewWithRunner(Config{Remote: "r"}, &fakeRunner{})
	if err == nil {
		t.Error("missing Label should fail")
	}
	_, err = NewWithRunner(Config{Label: "x"}, &fakeRunner{})
	if err == nil {
		t.Error("missing Remote should fail")
	}
}

func TestNewRequiresResolverWhenRefsProvided(t *testing.T) {
	t.Parallel()
	_, err := NewWithRunner(Config{
		Label: "x", Remote: "r",
		SecretRefs: map[string]string{"FOO": "env:FOO"},
	}, &fakeRunner{})
	if err == nil {
		t.Error("SecretRefs without resolver should fail")
	}
}

func TestStartSpawnsRcloneWithDefaults(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	m, err := NewWithRunner(Config{
		Label: "shared", Remote: "s3", Bucket: "lobs",
		MountRoot: t.TempDir(),
	}, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := r.last()
	if c.name != "rclone" {
		t.Errorf("cmd: %q", c.name)
	}
	// argv must include "mount", the source "s3:lobs", --daemon,
	// and the default vfs-cache-mode=full.
	joined := stringsJoin(c.args, " ")
	if !containsAll(joined, []string{"mount", "s3:lobs", "--daemon", "--vfs-cache-mode full"}) {
		t.Errorf("argv missing expected flags: %q", joined)
	}
}

func TestStartSecretsInjectedAsEnv(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	m, _ := NewWithRunner(Config{
		Label: "shared", Remote: "s3", Bucket: "lobs",
		MountRoot: t.TempDir(),
		SecretRefs: map[string]string{
			"ACCESS_KEY_ID":     "env:AWS_ACCESS_KEY_ID",
			"SECRET_ACCESS_KEY": "env:AWS_SECRET_ACCESS_KEY",
		},
		SecretResolver: func(ref string) (string, error) {
			switch ref {
			case "env:AWS_ACCESS_KEY_ID":
				return "AKIATEST", nil
			case "env:AWS_SECRET_ACCESS_KEY":
				return "sssshh", nil
			}
			return "", errors.New("unknown ref")
		},
	}, r)
	_ = m.Start(context.Background())

	c := r.last()
	var sawKey, sawSecret bool
	for _, e := range c.env {
		if e == "ACCESS_KEY_ID=AKIATEST" {
			sawKey = true
		}
		if e == "SECRET_ACCESS_KEY=sssshh" {
			sawSecret = true
		}
	}
	if !sawKey || !sawSecret {
		t.Errorf("expected resolved secrets in env; got %+v", c.env)
	}
}

func TestStartSecretResolutionFailureAborts(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	m, _ := NewWithRunner(Config{
		Label: "x", Remote: "r",
		MountRoot:  t.TempDir(),
		SecretRefs: map[string]string{"X": "env:X"},
		SecretResolver: func(_ string) (string, error) {
			return "", errors.New("not found")
		},
	}, r)
	err := m.Start(context.Background())
	if err == nil {
		t.Fatal("resolver failure should abort Start")
	}
	// rclone must NOT have been invoked.
	if len(r.calls) != 0 {
		t.Errorf("rclone invoked despite failed resolver: %+v", r.calls)
	}
}

func TestStopInvokesFusermount(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	m, _ := NewWithRunner(Config{
		Label: "x", Remote: "r", MountRoot: t.TempDir(),
	}, r)
	_ = m.Start(context.Background())

	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := r.last()
	if c.name != "fusermount" {
		t.Errorf("expected fusermount; got %q", c.name)
	}
	if len(c.args) == 0 || c.args[0] != "-u" {
		t.Errorf("expected -u; got %v", c.args)
	}
}

func TestStartPropagatesRunnerError(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{err: errors.New("rclone: access denied")}
	m, _ := NewWithRunner(Config{
		Label: "x", Remote: "r", MountRoot: t.TempDir(),
	}, r)
	if err := m.Start(context.Background()); err == nil {
		t.Error("spawn failure should propagate")
	}
}

// TestVFSCachePollCustom — operators can override the poll interval.
func TestVFSCachePollCustom(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	m, _ := NewWithRunner(Config{
		Label: "x", Remote: "r", MountRoot: t.TempDir(),
		VFSCachePoll: 15 * time.Second,
	}, r)
	_ = m.Start(context.Background())
	joined := stringsJoin(r.last().args, " ")
	if !containsAll(joined, []string{"--vfs-cache-poll-interval 15s"}) {
		t.Errorf("missing custom poll interval: %q", joined)
	}
}

// TestSplitOptionsAndSecrets — the Factory helper partitions the
// proto's Options map so *_ref keys become env-var secret refs
// and everything else becomes CLI flags.
func TestSplitOptionsAndSecrets(t *testing.T) {
	t.Parallel()
	opts, refs := splitOptionsAndSecrets(map[string]string{
		"buffer_size":       "64M",
		"access_key_id_ref": "env:AWS_KEY",
	})
	if opts["buffer_size"] != "64M" {
		t.Errorf("plain option lost: %v", opts)
	}
	if refs["ACCESS_KEY_ID"] != "env:AWS_KEY" {
		t.Errorf("ref not extracted: %v", refs)
	}
	if _, refLeaked := opts["access_key_id_ref"]; refLeaked {
		t.Error("ref should not appear in opts")
	}
}

// Helpers ------------------------------------------------------------

func stringsJoin(s []string, sep string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += sep
		}
		out += v
	}
	return out
}

func containsAll(haystack string, needles []string) bool {
	for _, n := range needles {
		if !stringContains(haystack, n) {
			return false
		}
	}
	return true
}

func stringContains(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
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

func TestRcloneMountImplementsInterface(t *testing.T) {
	t.Parallel()
	var _ mountLike = (*Mount)(nil)
}
