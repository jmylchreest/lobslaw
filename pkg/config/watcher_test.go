package config

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestWatchRequiresPaths(t *testing.T) {
	t.Parallel()
	err := Watch(t.Context(), WatchOptions{}, func([]fsnotify.Event) {})
	if err == nil {
		t.Fatal("empty paths should fail")
	}
}

func TestWatchFiresOnWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.toml")
	if err := os.WriteFile(path, []byte("key = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var fires int32
	done := make(chan struct{}, 1)
	go func() {
		_ = Watch(ctx, WatchOptions{
			Paths:    []string{path},
			Debounce: 50 * time.Millisecond,
		}, func([]fsnotify.Event) {
			atomic.AddInt32(&fires, 1)
			select {
			case done <- struct{}{}:
			default:
			}
		})
	}()

	// Give the watcher a moment to register the parent dir.
	time.Sleep(100 * time.Millisecond)

	if err := os.WriteFile(path, []byte("key = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not fire within 2s of write")
	}

	if got := atomic.LoadInt32(&fires); got != 1 {
		t.Errorf("fires = %d; want 1", got)
	}
}

// TestWatchDebouncesBursts — a rapid sequence of writes within the
// debounce window should collapse to a single onChange call.
func TestWatchDebouncesBursts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.toml")
	if err := os.WriteFile(path, []byte("x=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var (
		fires  int32
		mu     sync.Mutex
		events []fsnotify.Event
	)
	done := make(chan struct{}, 1)
	go func() {
		_ = Watch(ctx, WatchOptions{
			Paths:    []string{path},
			Debounce: 150 * time.Millisecond,
		}, func(batch []fsnotify.Event) {
			atomic.AddInt32(&fires, 1)
			mu.Lock()
			events = append(events, batch...)
			mu.Unlock()
			select {
			case done <- struct{}{}:
			default:
			}
		})
	}()

	time.Sleep(100 * time.Millisecond)

	for i := range 5 {
		if err := os.WriteFile(path, []byte{byte('0' + i)}, 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not fire within 2s")
	}

	// Give the debounce a bit more time; if another fire slips
	// through, we'd see fires > 1.
	time.Sleep(200 * time.Millisecond)

	if got := atomic.LoadInt32(&fires); got != 1 {
		t.Errorf("fires = %d; want 1 (debounce should collapse bursts)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 {
		t.Error("onChange should receive at least one event")
	}
}

// TestWatchIgnoresSiblingFiles — writes to other files in the same
// parent directory must NOT trigger the callback.
func TestWatchIgnoresSiblingFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	watched := filepath.Join(dir, "cfg.toml")
	sibling := filepath.Join(dir, "other.toml")
	if err := os.WriteFile(watched, []byte("x=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var fires int32
	go func() {
		_ = Watch(ctx, WatchOptions{
			Paths:    []string{watched},
			Debounce: 50 * time.Millisecond,
		}, func([]fsnotify.Event) {
			atomic.AddInt32(&fires, 1)
		})
	}()

	time.Sleep(100 * time.Millisecond)

	if err := os.WriteFile(sibling, []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)

	if got := atomic.LoadInt32(&fires); got != 0 {
		t.Errorf("watcher fired on sibling file change; fires=%d", got)
	}
}

func TestWatchCancelsOnContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.toml")
	_ = os.WriteFile(path, []byte("x=0\n"), 0o600)

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- Watch(ctx, WatchOptions{
			Paths:    []string{path},
			Debounce: 50 * time.Millisecond,
		}, func([]fsnotify.Event) {})
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil || err != context.Canceled {
			t.Errorf("expected context.Canceled; got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher did not exit on cancel")
	}
}

func TestAtomicConfigLoadStore(t *testing.T) {
	t.Parallel()
	type cfg struct{ N int }
	var a AtomicConfig[cfg]
	if a.Load() != nil {
		t.Error("zero value should Load nil")
	}
	a.Store(&cfg{N: 42})
	got := a.Load()
	if got == nil || got.N != 42 {
		t.Errorf("Load after Store = %+v", got)
	}
	a.Store(&cfg{N: 99})
	if a.Load().N != 99 {
		t.Error("second Store should replace")
	}
}
