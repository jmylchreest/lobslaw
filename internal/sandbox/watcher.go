package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// PolicySink receives the result of a policy reload. Decoupled from
// *compute.Registry to avoid an import cycle (compute already imports
// sandbox). Registry satisfies this interface naturally.
type PolicySink interface {
	SetPolicy(name string, p *Policy)
}

// DefaultDebounce is how long we wait after the last filesystem
// event before triggering a reload. Editors like vim fire several
// events per save (write-swap-rename); 250ms comfortably coalesces
// without feeling sluggish when an operator deliberately drops in
// a new policy file.
const DefaultDebounce = 250 * time.Millisecond

// Watcher wraps an fsnotify watcher with debouncing and the sandbox
// package's load-and-apply semantics. Exactly one reload runs at a
// time; back-to-back events within the debounce window coalesce
// into a single reload firing at window-end.
//
// Accepts a list of directories and watches all of them (plus any
// _presets/ subdir under each). Any event in any dir triggers a
// full re-merge via LoadPolicyDirs — "later dir wins" semantics
// remain stable across reloads.
//
// Lifecycle: NewWatcher → Start(ctx) → cancel ctx to stop. Stop is
// also callable to wind down without a ctx.
type Watcher struct {
	dirs     []string
	sink     PolicySink
	opts     LoadOptions
	logger   *slog.Logger
	debounce time.Duration

	// knownTools tracks which tool names were populated by the most
	// recent successful reload. Drives "policy file deleted" semantics:
	// any tool present here but NOT in the new Policies map gets
	// SetPolicy(nil) so the fleet default takes over again.
	mu         sync.Mutex
	knownTools map[string]struct{}
}

// NewWatcher constructs a ready-to-Start Watcher for a single dir.
// Kept as a thin convenience wrapper over NewWatcherMulti for
// callers that don't need multi-dir semantics.
func NewWatcher(dir string, sink PolicySink, opts LoadOptions, debounce time.Duration) *Watcher {
	return NewWatcherMulti([]string{dir}, sink, opts, debounce)
}

// NewWatcherMulti is the multi-dir constructor. Doesn't touch the
// filesystem or start any goroutines — Start does that. opts is
// applied to each reload with the same semantics as a direct call
// to LoadPolicyDirs.
func NewWatcherMulti(dirs []string, sink PolicySink, opts LoadOptions, debounce time.Duration) *Watcher {
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Watcher{
		dirs:       append([]string(nil), dirs...),
		sink:       sink,
		opts:       opts,
		logger:     opts.Logger,
		debounce:   debounce,
		knownTools: make(map[string]struct{}),
	}
}

// Start runs the watcher loop until ctx is cancelled. A first-pass
// load runs synchronously before subscribing to fsnotify events so
// boot-time behaviour is identical to calling LoadPolicyDir+apply
// manually — the watcher only exists to keep things fresh.
//
// Returns an error only when the initial load or fsnotify setup
// fails; post-start reload errors are logged, not returned.
func (w *Watcher) Start(ctx context.Context) error {
	if _, err := w.reloadNow(); err != nil {
		return fmt.Errorf("initial load: %w", err)
	}

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify new: %w", err)
	}
	// Subscribe to every configured dir + its _presets subdir.
	// Missing dirs are skipped (LoadPolicyDirs handles them as no-ops
	// at load time, but fsnotify.Add fails hard on missing paths).
	for _, dir := range w.dirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			w.logger.Info("sandbox watcher: skipping missing policy dir",
				"path", dir)
			continue
		}
		if err := fw.Add(dir); err != nil {
			w.logger.Warn("sandbox watcher: couldn't watch dir",
				"path", dir, "error", err)
			continue
		}
		// _presets subdir — fsnotify doesn't recurse on Linux.
		presetsDir := filepath.Join(dir, PresetSubdir)
		if _, err := os.Stat(presetsDir); err == nil {
			if err := fw.Add(presetsDir); err != nil {
				w.logger.Warn("sandbox watcher: couldn't watch presets subdir",
					"path", presetsDir, "error", err)
			}
		}
	}

	go w.loop(ctx, fw)
	return nil
}

// loop pumps fsnotify events through a debounce timer and triggers
// reloadNow at debounce-window-end. Runs until ctx is cancelled.
func (w *Watcher) loop(ctx context.Context, fw *fsnotify.Watcher) {
	defer fw.Close()

	// pending is non-nil when a reload is scheduled. Refreshing it
	// on every event gives us the "quiet for debounce ms" behaviour.
	var pending *time.Timer
	fire := make(chan struct{}, 1)

	resetTimer := func() {
		if pending != nil {
			pending.Stop()
		}
		pending = time.AfterFunc(w.debounce, func() {
			select {
			case fire <- struct{}{}:
			default:
			}
		})
	}

	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-fw.Errors:
			if !ok {
				return
			}
			w.logger.Warn("sandbox watcher: fsnotify error", "error", err)
		case ev, ok := <-fw.Events:
			if !ok {
				return
			}
			// Filter to events that actually change policy content.
			// CHMOD alone (e.g. touch) isn't interesting; only
			// create/write/remove/rename warrant a reload.
			const interesting = fsnotify.Create | fsnotify.Write |
				fsnotify.Remove | fsnotify.Rename
			if ev.Op&interesting == 0 {
				continue
			}
			resetTimer()
		case <-fire:
			if _, err := w.reloadNow(); err != nil {
				w.logger.Error("sandbox watcher: reload failed",
					"dirs", w.dirs, "error", err)
			}
		}
	}
}

// reloadNow loads the directory once, applies each policy to the
// sink, and clears policies for tools that disappeared since the
// previous load. Returns the LoadResult so callers / tests can
// inspect what happened.
//
// Concurrency: Start/loop guarantees reloads are serial (one timer
// slot), so this method takes the mutex only to protect knownTools.
func (w *Watcher) reloadNow() (*LoadResult, error) {
	result, err := LoadPolicyDirs(w.dirs, w.opts)
	if err != nil {
		return nil, err
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// Apply new/updated policies.
	newKnown := make(map[string]struct{}, len(result.Policies))
	for name, policy := range result.Policies {
		w.sink.SetPolicy(name, policy)
		newKnown[name] = struct{}{}
	}
	// Clear policies for tools that vanished since last reload.
	for name := range w.knownTools {
		if _, stillThere := newKnown[name]; !stillThere {
			w.sink.SetPolicy(name, nil)
			w.logger.Info("sandbox watcher: cleared policy for removed tool",
				"tool", name)
		}
	}
	w.knownTools = newKnown

	if n := len(result.Rejected); n > 0 {
		w.logger.Warn("sandbox watcher: rejected files",
			"count", n, "names", result.Rejected)
	}
	if n := len(result.OverriddenBuiltins); n > 0 {
		w.logger.Info("sandbox watcher: operator presets shadow built-ins",
			"names", result.OverriddenBuiltins)
	}
	return result, nil
}
