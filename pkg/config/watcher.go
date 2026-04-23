package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatchOptions configures Watch. Paths are individual files to
// observe; the watcher registers their parent directories with
// fsnotify so rename-then-replace editor saves still deliver events.
// Debounce collapses bursts of fs events into one onChange call
// (1.5s per PLAN.md § 11.2); zero picks DefaultDebounce. Logger is
// optional.
type WatchOptions struct {
	Paths    []string
	Debounce time.Duration
	Logger   *slog.Logger
}

// DefaultDebounce is the debounce window used when WatchOptions
// leaves it zero.
const DefaultDebounce = 1500 * time.Millisecond

// Watch observes the supplied paths for change events and fires
// onChange after Debounce of quiet. Blocks until ctx is cancelled;
// returns ctx.Err() on cancel or a setup error.
//
// Rather than parsing inside this package (which would couple the
// watcher to a specific config layout), onChange receives the raw
// event list for the quiet window. Callers choose what to re-read
// — whole-config reload for config.toml, SOUL.md-only reload for
// personality tweaks.
func Watch(ctx context.Context, opts WatchOptions, onChange func([]fsnotify.Event)) error {
	if len(opts.Paths) == 0 {
		return errors.New("config.Watch: at least one path required")
	}
	debounce := opts.Debounce
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("config.Watch: %w", err)
	}
	defer watcher.Close()

	// Watch each file's parent directory so editors that replace via
	// rename pattern (vim, many IDEs) don't break the watch.
	seen := make(map[string]bool)
	targets := make(map[string]bool, len(opts.Paths))
	for _, p := range opts.Paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("config.Watch: abs %q: %w", p, err)
		}
		targets[abs] = true
		dir := filepath.Dir(abs)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if err := watcher.Add(dir); err != nil {
			return fmt.Errorf("config.Watch: watch %q: %w", dir, err)
		}
	}

	// Stopped timer: its channel blocks until we Reset it. Using a
	// pre-created timer (instead of creating on first event) keeps
	// the select uniform — we never need a nil-channel branch.
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	var pending []fsnotify.Event

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err, ok := <-watcher.Errors:
			if !ok {
				return errors.New("config.Watch: watcher errors channel closed")
			}
			log.Warn("config watcher error", "err", err)

		case ev, ok := <-watcher.Events:
			if !ok {
				return errors.New("config.Watch: watcher events channel closed")
			}
			absPath, err := filepath.Abs(ev.Name)
			if err != nil {
				continue
			}
			if !targets[absPath] {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			pending = append(pending, ev)
			// Reset: stop-then-start pattern avoids the documented
			// race where Reset on an already-fired-but-undrained
			// timer can cause a double-deliver.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(debounce)

		case <-timer.C:
			if len(pending) == 0 {
				continue
			}
			batch := pending
			pending = nil
			onChange(batch)
		}
	}
}
