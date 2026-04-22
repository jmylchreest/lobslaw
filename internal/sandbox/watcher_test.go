package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// recordingSink captures SetPolicy calls for assertion. The watcher
// fires from a goroutine so access is guarded by a mutex.
type recordingSink struct {
	mu    sync.Mutex
	calls map[string][]*Policy // tool → history of Policy values (nil included)
}

func newRecordingSink() *recordingSink {
	return &recordingSink{calls: make(map[string][]*Policy)}
}

func (s *recordingSink) SetPolicy(name string, p *Policy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[name] = append(s.calls[name], p)
}

func (s *recordingSink) lastFor(name string) (*Policy, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.calls[name]
	if !ok || len(h) == 0 {
		return nil, false
	}
	return h[len(h)-1], true
}

func (s *recordingSink) countFor(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls[name])
}

// waitFor polls f until it returns true or timeout elapses. Used to
// synchronise tests with the watcher's async reload goroutine
// without baking in flaky sleeps.
func waitFor(t *testing.T, timeout time.Duration, f func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestWatcherInitialLoadPopulatesSink confirms Start does a synchronous
// initial load before wiring up fsnotify — boot-time behaviour is
// identical to a direct LoadPolicyDir call, so nothing racy needs to
// be awaited for the first set of policies.
func TestWatcherInitialLoadPopulatesSink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePolicyFile(t, filepath.Join(dir, "git.toml"), `
name = "git"
paths = ["/tmp:rw"]
`)

	sink := newRecordingSink()
	w := NewWatcher(dir, sink, LoadOptions{}, 0)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if p, ok := sink.lastFor("git"); !ok || p == nil {
		t.Errorf("initial load should have set 'git' policy; history=%v", sink.calls["git"])
	}
}

// TestWatcherFileCreateTriggersReload — the normal hot-reload flow:
// operator drops a new policy file, inotify event fires, debounce
// window elapses, reload runs, sink sees the new policy.
func TestWatcherFileCreateTriggersReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	sink := newRecordingSink()
	w := NewWatcher(dir, sink, LoadOptions{}, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	writePolicyFile(t, filepath.Join(dir, "git.toml"), `
name = "git"
paths = ["/tmp:rw"]
`)

	if !waitFor(t, 2*time.Second, func() bool {
		_, ok := sink.lastFor("git")
		return ok
	}) {
		t.Fatalf("reload didn't pick up new file within timeout; sink=%v", sink.calls)
	}
}

// TestWatcherFileDeleteClearsPolicy — symmetric to Create: when a
// file disappears, the watcher clears the per-tool policy so the
// fleet default (or nil) takes over again.
func TestWatcherFileDeleteClearsPolicy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "git.toml")
	writePolicyFile(t, path, `
name = "git"
paths = ["/tmp:rw"]
`)

	sink := newRecordingSink()
	w := NewWatcher(dir, sink, LoadOptions{}, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Initial load set the policy; now delete and expect clear.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if !waitFor(t, 2*time.Second, func() bool {
		p, ok := sink.lastFor("git")
		return ok && p == nil
	}) {
		t.Fatalf("delete didn't clear policy within timeout; history=%v", sink.calls["git"])
	}
}

// TestWatcherDebounceCoalescesBurst proves that a burst of events
// (e.g. editor's write-rename-fsync sequence) produces ONE reload,
// not many. Counted by the number of times the sink saw "git" set.
func TestWatcherDebounceCoalescesBurst(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "git.toml")
	writePolicyFile(t, path, `
name = "git"
paths = ["/tmp:rw"]
`)

	sink := newRecordingSink()
	// Deliberately long debounce — any burst in the next 150ms
	// should collapse into ONE reload.
	w := NewWatcher(dir, sink, LoadOptions{}, 150*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	initial := sink.countFor("git")

	// Rapid-fire rewrites. Each fires at least one fsnotify event;
	// debounce should coalesce all of them into a single reload.
	for i := range 5 {
		writePolicyFile(t, path, `
name = "git"
paths = ["/tmp:rw"]
description = "rev `+string(rune('A'+i))+`"
`)
		time.Sleep(10 * time.Millisecond)
	}

	// Wait for at least one reload post-burst. We can't assert
	// "exactly 1" because fsnotify timing is OS-dependent — but we
	// can assert it's fewer than the number of writes.
	if !waitFor(t, 2*time.Second, func() bool {
		return sink.countFor("git") > initial
	}) {
		t.Fatal("no reload observed after burst")
	}
	time.Sleep(300 * time.Millisecond) // let any trailing reloads land

	n := sink.countFor("git") - initial
	if n >= 5 {
		t.Errorf("debounce didn't coalesce: got %d reloads for 5 writes", n)
	}
}

// TestWatcherContextCancelStopsReloads — the lifecycle guarantee:
// once ctx is cancelled, no further reloads happen even when
// subsequent filesystem events fire.
func TestWatcherContextCancelStopsReloads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	sink := newRecordingSink()
	w := NewWatcher(dir, sink, LoadOptions{}, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cancel()
	time.Sleep(100 * time.Millisecond) // let goroutine shut down

	beforeWrite := sink.countFor("git")
	writePolicyFile(t, filepath.Join(dir, "git.toml"), `
name = "git"
paths = ["/tmp:rw"]
`)
	// Give plenty of time to see if a stray reload fires.
	time.Sleep(300 * time.Millisecond)

	if got := sink.countFor("git"); got != beforeWrite {
		t.Errorf("post-cancel writes should not trigger reloads; count before=%d after=%d",
			beforeWrite, got)
	}
}

// TestWatcherPermRejectIsLoggedNotFatal — a file that fails the perm
// check triggers a Warn log (not a reload error); unaffected sibling
// files still load. Regression guard against "one bad file poisons
// the whole watcher".
func TestWatcherPermRejectIsLoggedNotFatal(t *testing.T) {
	if runtime_GOOS() == "windows" {
		t.Skip("mode-bit checks don't apply on Windows")
	}
	t.Parallel()
	dir := t.TempDir()
	good := filepath.Join(dir, "good.toml")
	bad := filepath.Join(dir, "bad.toml")
	writePolicyFile(t, good, `name = "good"`+"\n"+`paths = ["/tmp:rw"]`)
	writePolicyFile(t, bad, `name = "bad"`+"\n"+`paths = ["/tmp:rw"]`)
	_ = os.Chmod(bad, 0o666)

	sink := newRecordingSink()
	w := NewWatcher(dir, sink, LoadOptions{}, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, ok := sink.lastFor("good"); !ok {
		t.Error("good file should load even when sibling is rejected")
	}
	if p, ok := sink.lastFor("bad"); ok && p != nil {
		t.Error("bad file should not have loaded")
	}
}
