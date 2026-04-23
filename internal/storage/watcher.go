package storage

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

// EventOp enumerates the kinds of change the watcher emits.
type EventOp int

const (
	// Initial fires once per existing file when a subscription
	// starts — lets subscribers handle startup + runtime
	// uniformly.
	EventInitial EventOp = iota
	EventCreate
	EventWrite
	EventRemove
	EventRename
)

func (e EventOp) String() string {
	switch e {
	case EventInitial:
		return "initial"
	case EventCreate:
		return "create"
	case EventWrite:
		return "write"
	case EventRemove:
		return "remove"
	case EventRename:
		return "rename"
	default:
		return "unknown"
	}
}

// Event is the surfaced change record. Stat is a best-effort
// os.FileInfo — nil for Remove (the file is gone) and may be nil
// if we race the fsnotify event against a concurrent rename.
type Event struct {
	Path string
	Op   EventOp
	Stat os.FileInfo
}

// WatchOpts tunes one subscription. Zero values pick sensible
// defaults: non-recursive, poll using the backend-specific default
// interval, no include/exclude filtering.
type WatchOpts struct {
	// Recursive descends into subdirectories. The watcher walks the
	// current tree at subscription time (emitting Initial events) and
	// dynamically adds kernel watches for new subdirectories as
	// Create events arrive.
	Recursive bool

	// PollInterval is how often the periodic scan runs. 0 means "use
	// the backend default" (computed by WatchOn); negative disables
	// polling entirely (fsnotify-only — appropriate for local-only
	// workflows).
	PollInterval time.Duration

	// Include / Exclude are glob patterns matched against the
	// filename relative to the watch root. Include-empty means
	// "match all." Exclude runs after Include.
	Include []string
	Exclude []string

	// Logger is used for diagnostic warn-level events (fsnotify
	// errors, scan glitches). Nil → slog.Default().
	Logger *slog.Logger
}

// Watch subscribes to changes at the given label's root. Delegates
// to WatchOn with the Manager-resolved path and a backend-aware
// default PollInterval.
//
// The returned channel closes when ctx is cancelled. Unread events
// are dropped (non-blocking send) so a slow subscriber can't stall
// the scan loop.
func (m *Manager) Watch(ctx context.Context, label string, opts WatchOpts) (<-chan Event, error) {
	m.mu.RLock()
	mount, ok := m.mounts[label]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = defaultPollInterval(mount.Backend())
	}
	return WatchOn(ctx, mount.Path(), opts)
}

// WatchOn is the path-level primitive that Manager.Watch delegates
// to. Exported so tests and callers with a pre-resolved path (e.g.
// skills registry watching the skill root) can use the same
// machinery without registering a mount first.
func WatchOn(ctx context.Context, root string, opts WatchOpts) (<-chan Event, error) {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("storage: watch root %q must be absolute", root)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	events := make(chan Event, 128)
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("storage: fsnotify: %w", err)
	}

	sub := &subscription{
		root:   root,
		opts:   opts,
		events: events,
		fs:     w,
		seen:   make(map[string]fileState),
		log:    opts.Logger,
	}
	if err := sub.prime(ctx); err != nil {
		_ = w.Close()
		close(events)
		return nil, err
	}
	go sub.run(ctx)
	return events, nil
}

// defaultPollInterval encodes the backend-aware defaults from the
// Phase 9 design: local mounts rely on fsnotify only; nfs/rclone
// need periodic scan to catch remote-origin writes.
func defaultPollInterval(backend string) time.Duration {
	switch backend {
	case "local":
		return -1 // disable polling; fsnotify is complete
	case "nfs", "rclone":
		return 5 * time.Minute
	default:
		return 5 * time.Minute
	}
}

// fileState is the per-path snapshot the scanner diffs on each
// poll. Size + mtime is a good-enough identity for "did anything
// interesting happen?" without reading the full content.
type fileState struct {
	size  int64
	mtime time.Time
}

type subscription struct {
	root   string
	opts   WatchOpts
	events chan Event
	fs     *fsnotify.Watcher
	log    *slog.Logger

	mu   sync.Mutex
	seen map[string]fileState
}

// prime walks the root and emits Initial events for every matching
// file. Registers kernel watches on directories encountered.
func (s *subscription) prime(_ context.Context) error {
	return filepath.Walk(s.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			if !s.opts.Recursive && path != s.root {
				return filepath.SkipDir
			}
			if err := s.fs.Add(path); err != nil {
				s.log.Warn("storage: fsnotify.Add failed", "path", path, "err", err)
			}
			return nil
		}
		if !s.matches(path) {
			return nil
		}
		s.mu.Lock()
		s.seen[path] = fileState{size: info.Size(), mtime: info.ModTime()}
		s.mu.Unlock()
		s.emit(Event{Path: path, Op: EventInitial, Stat: info})
		return nil
	})
}

// run is the subscription's long-lived goroutine. Composes the
// fsnotify event feed with the periodic scanner. Exits cleanly on
// ctx cancel.
func (s *subscription) run(ctx context.Context) {
	defer close(s.events)
	defer func() { _ = s.fs.Close() }()

	var pollTick <-chan time.Time
	if s.opts.PollInterval > 0 {
		t := time.NewTicker(s.opts.PollInterval)
		defer t.Stop()
		pollTick = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-s.fs.Events:
			if !ok {
				return
			}
			s.handleFSEvent(ev)
		case err, ok := <-s.fs.Errors:
			if !ok {
				return
			}
			s.log.Warn("storage: fsnotify error", "err", err)
		case <-pollTick:
			s.scanOnce()
		}
	}
}

// handleFSEvent translates an fsnotify event into our Event shape.
// Register kernel watches on newly-created subdirectories when
// Recursive is set.
func (s *subscription) handleFSEvent(ev fsnotify.Event) {
	info, statErr := os.Stat(ev.Name)
	switch {
	case ev.Op&fsnotify.Create != 0:
		if statErr == nil && info.IsDir() {
			if s.opts.Recursive {
				if err := s.fs.Add(ev.Name); err != nil {
					s.log.Warn("storage: fsnotify.Add (recursive)", "path", ev.Name, "err", err)
				}
				// Walk the new subtree so initial content gets events too.
				_ = filepath.Walk(ev.Name, func(p string, i os.FileInfo, e error) error {
					if e != nil || i.IsDir() || !s.matches(p) {
						return nil
					}
					s.mu.Lock()
					s.seen[p] = fileState{size: i.Size(), mtime: i.ModTime()}
					s.mu.Unlock()
					s.emit(Event{Path: p, Op: EventCreate, Stat: i})
					return nil
				})
			}
			return
		}
		if !s.matches(ev.Name) {
			return
		}
		if statErr == nil {
			s.mu.Lock()
			s.seen[ev.Name] = fileState{size: info.Size(), mtime: info.ModTime()}
			s.mu.Unlock()
		}
		s.emit(Event{Path: ev.Name, Op: EventCreate, Stat: info})
	case ev.Op&fsnotify.Write != 0:
		if !s.matches(ev.Name) {
			return
		}
		if statErr == nil {
			s.mu.Lock()
			s.seen[ev.Name] = fileState{size: info.Size(), mtime: info.ModTime()}
			s.mu.Unlock()
		}
		s.emit(Event{Path: ev.Name, Op: EventWrite, Stat: info})
	case ev.Op&fsnotify.Remove != 0:
		if !s.matches(ev.Name) {
			return
		}
		s.mu.Lock()
		delete(s.seen, ev.Name)
		s.mu.Unlock()
		s.emit(Event{Path: ev.Name, Op: EventRemove})
	case ev.Op&fsnotify.Rename != 0:
		if !s.matches(ev.Name) {
			return
		}
		s.mu.Lock()
		delete(s.seen, ev.Name)
		s.mu.Unlock()
		s.emit(Event{Path: ev.Name, Op: EventRename})
	}
}

// scanOnce does a full tree walk, diffing against s.seen to catch
// writes that fsnotify missed (remote-origin writes on nfs/rclone
// mounts, or events lost to watcher backlog). Emits Create/Write/
// Remove for the differences.
func (s *subscription) scanOnce() {
	now := make(map[string]fileState, len(s.seen))
	err := filepath.Walk(s.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if !s.opts.Recursive && path != s.root {
				return filepath.SkipDir
			}
			return nil
		}
		if !s.matches(path) {
			return nil
		}
		now[path] = fileState{size: info.Size(), mtime: info.ModTime()}
		s.mu.Lock()
		prev, existed := s.seen[path]
		s.seen[path] = now[path]
		s.mu.Unlock()
		switch {
		case !existed:
			s.emit(Event{Path: path, Op: EventCreate, Stat: info})
		case prev.size != now[path].size || !prev.mtime.Equal(now[path].mtime):
			s.emit(Event{Path: path, Op: EventWrite, Stat: info})
		}
		return nil
	})
	if err != nil {
		s.log.Warn("storage: scan walk failed", "root", s.root, "err", err)
	}

	s.mu.Lock()
	for path := range s.seen {
		if _, stillThere := now[path]; !stillThere {
			delete(s.seen, path)
			s.mu.Unlock()
			s.emit(Event{Path: path, Op: EventRemove})
			s.mu.Lock()
		}
	}
	s.mu.Unlock()
}

// matches applies the include/exclude globs relative to s.root.
// Include-empty means "match all." Exclude is evaluated after
// include so an exclude can veto an included match.
func (s *subscription) matches(path string) bool {
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	included := len(s.opts.Include) == 0
	for _, pat := range s.opts.Include {
		if ok, _ := filepath.Match(pat, rel); ok {
			included = true
			break
		}
		if ok, _ := filepath.Match(pat, filepath.Base(path)); ok {
			included = true
			break
		}
	}
	if !included {
		return false
	}
	for _, pat := range s.opts.Exclude {
		if ok, _ := filepath.Match(pat, rel); ok {
			return false
		}
		if ok, _ := filepath.Match(pat, filepath.Base(path)); ok {
			return false
		}
	}
	return true
}

// emit sends non-blockingly. A full channel drops the event and
// logs a warn — better than deadlocking the scan/fsnotify feed on
// a slow subscriber.
func (s *subscription) emit(ev Event) {
	select {
	case s.events <- ev:
	default:
		s.log.Warn("storage: watcher channel full — dropping event",
			"op", ev.Op.String(), "path", ev.Path)
	}
}

