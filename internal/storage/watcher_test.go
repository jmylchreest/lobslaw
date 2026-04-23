package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// drainEvents collects events for the given duration so tests can
// assert on the full stream rather than racing the subscription
// goroutine.
func drainEvents(ch <-chan Event, window time.Duration) []Event {
	deadline := time.After(window)
	var out []Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
}

// waitForEvent blocks until an event matching op+suffix arrives or
// timeout. Returns the event or an empty struct on timeout. Lets
// tests synchronise without hard-coded sleeps.
func waitForEvent(ch <-chan Event, op EventOp, pathSuffix string, timeout time.Duration) Event {
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return Event{}
			}
			if ev.Op == op && filepathHasSuffix(ev.Path, pathSuffix) {
				return ev
			}
		case <-deadline:
			return Event{}
		}
	}
}

func filepathHasSuffix(p, suffix string) bool {
	if suffix == "" {
		return true
	}
	return len(p) >= len(suffix) && p[len(p)-len(suffix):] == suffix
}

func TestWatcherEmitsInitialForExistingFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bye"), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := WatchOn(ctx, dir, WatchOpts{PollInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	got := drainEvents(ch, 150*time.Millisecond)
	initialCount := 0
	for _, ev := range got {
		if ev.Op == EventInitial {
			initialCount++
		}
	}
	if initialCount != 2 {
		t.Errorf("expected 2 Initial events; got %d in %+v", initialCount, got)
	}
}

func TestWatcherRejectsRelativePath(t *testing.T) {
	t.Parallel()
	_, err := WatchOn(context.Background(), "./relative", WatchOpts{})
	if err == nil {
		t.Error("relative root should fail")
	}
}

func TestWatcherEmitsCreateOnNewFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := WatchOn(ctx, dir, WatchOpts{PollInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	drainEvents(ch, 50*time.Millisecond) // drain any prime output

	// Write a new file; wait for the Create event.
	_ = os.WriteFile(filepath.Join(dir, "new.txt"), []byte("created"), 0o644)

	ev := waitForEvent(ch, EventCreate, "new.txt", 2*time.Second)
	if ev.Path == "" {
		t.Fatal("no Create event observed within 2s")
	}
}

func TestWatcherEmitsWriteOnModify(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "tracked.txt")
	_ = os.WriteFile(target, []byte("initial"), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := WatchOn(ctx, dir, WatchOpts{PollInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	drainEvents(ch, 50*time.Millisecond)

	_ = os.WriteFile(target, []byte("modified content"), 0o644)
	ev := waitForEvent(ch, EventWrite, "tracked.txt", 2*time.Second)
	if ev.Path == "" {
		t.Fatal("no Write event observed within 2s")
	}
}

func TestWatcherEmitsRemove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "goner.txt")
	_ = os.WriteFile(target, []byte("x"), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := WatchOn(ctx, dir, WatchOpts{PollInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	drainEvents(ch, 50*time.Millisecond)

	_ = os.Remove(target)
	ev := waitForEvent(ch, EventRemove, "goner.txt", 2*time.Second)
	if ev.Path == "" {
		t.Fatal("no Remove event observed within 2s")
	}
}

// TestWatcherIncludeGlob — when Include patterns are set, files that
// don't match are silently ignored.
func TestWatcherIncludeGlob(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "keep.yaml"), []byte("y"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "drop.json"), []byte("j"), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := WatchOn(ctx, dir, WatchOpts{
		PollInterval: -1,
		Include:      []string{"*.yaml"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := drainEvents(ch, 200*time.Millisecond)

	var sawKeep, sawDrop bool
	for _, ev := range got {
		if filepathHasSuffix(ev.Path, "keep.yaml") {
			sawKeep = true
		}
		if filepathHasSuffix(ev.Path, "drop.json") {
			sawDrop = true
		}
	}
	if !sawKeep {
		t.Error("yaml file should be observed")
	}
	if sawDrop {
		t.Error("json file should be filtered out")
	}
}

func TestWatcherExcludeGlob(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "keep.yaml"), []byte("y"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "tmp.tmp"), []byte("t"), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _ := WatchOn(ctx, dir, WatchOpts{
		PollInterval: -1,
		Exclude:      []string{"*.tmp"},
	})
	got := drainEvents(ch, 200*time.Millisecond)

	var sawTmp bool
	for _, ev := range got {
		if filepathHasSuffix(ev.Path, "tmp.tmp") {
			sawTmp = true
		}
	}
	if sawTmp {
		t.Error("*.tmp file should be excluded")
	}
}

// TestWatcherRecursiveEmitsInitialFromSubdirs — recursive watch
// walks the tree at subscription time.
func TestWatcherRecursiveEmitsInitialFromSubdirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sub := filepath.Join(dir, "inner")
	_ = os.Mkdir(sub, 0o755)
	_ = os.WriteFile(filepath.Join(sub, "nested.txt"), []byte("n"), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _ := WatchOn(ctx, dir, WatchOpts{PollInterval: -1, Recursive: true})
	got := drainEvents(ch, 200*time.Millisecond)

	var sawNested bool
	for _, ev := range got {
		if ev.Op == EventInitial && filepathHasSuffix(ev.Path, "nested.txt") {
			sawNested = true
		}
	}
	if !sawNested {
		t.Errorf("recursive prime should emit for nested file; got %+v", got)
	}
}

// TestWatcherPollingDetectsRemoteStyleWrite — simulates an "nfs
// wrote this, fsnotify didn't fire" scenario by writing with mtime
// in the past then bumping it. The scan loop diffs mtime and emits
// Write even though no fsnotify event arrived.
func TestWatcherPollingDetectsRemoteStyleWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "polled.txt")
	_ = os.WriteFile(target, []byte("v1"), 0o644)
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(target, past, past)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := WatchOn(ctx, dir, WatchOpts{PollInterval: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	drainEvents(ch, 50*time.Millisecond)

	// Bump mtime without fsnotify firing — emulate remote write.
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(target, future, future)

	ev := waitForEvent(ch, EventWrite, "polled.txt", 2*time.Second)
	if ev.Path == "" {
		t.Fatal("scan loop did not surface mtime-only change")
	}
}

// TestWatcherStopOnContextCancel — cancelling ctx closes the
// events channel and exits the goroutine cleanly.
func TestWatcherStopOnContextCancel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := WatchOn(ctx, dir, WatchOpts{PollInterval: -1})
	if err != nil {
		t.Fatal(err)
	}

	cancel()
	// Drain until the channel closes.
	closed := false
	deadline := time.After(2 * time.Second)
	for !closed {
		select {
		case _, ok := <-ch:
			if !ok {
				closed = true
			}
		case <-deadline:
			t.Fatal("watcher didn't close channel within 2s of cancel")
		}
	}
}

// TestManagerWatchResolvesLabel — Manager.Watch pipes through to
// WatchOn using the label's Path.
func TestManagerWatchResolvesLabel(t *testing.T) {
	t.Parallel()
	m := NewManager()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("s"), 0o644)

	mt := &fakeMount{label: "wlbl", backend: "local", path: dir}
	_ = m.Register(context.Background(), mt)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := m.Watch(ctx, "wlbl", WatchOpts{PollInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	got := drainEvents(ch, 200*time.Millisecond)
	if len(got) == 0 {
		t.Error("Manager.Watch should emit at least one Initial event")
	}
}

func TestManagerWatchUnknownLabelIsErrNotFound(t *testing.T) {
	t.Parallel()
	m := NewManager()
	_, err := m.Watch(context.Background(), "missing", WatchOpts{})
	if err != ErrNotFound {
		t.Errorf("want ErrNotFound; got %v", err)
	}
}

func TestEventOpStrings(t *testing.T) {
	t.Parallel()
	cases := map[EventOp]string{
		EventInitial: "initial",
		EventCreate:  "create",
		EventWrite:   "write",
		EventRemove:  "remove",
		EventRename:  "rename",
	}
	for op, want := range cases {
		if got := op.String(); got != want {
			t.Errorf("%d → %q, want %q", op, got, want)
		}
	}
}
