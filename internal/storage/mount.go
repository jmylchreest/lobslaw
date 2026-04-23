package storage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrNotFound surfaces when Resolve is asked about a label that
// isn't in the manager's mount set.
var ErrNotFound = errors.New("storage: mount label not found")

// Mount is a runtime handle on one materialised mount. Implementations
// live in sibling packages (internal/storage/local, nfs, rclone);
// the core Manager only knows them through this interface.
//
// Lifecycle: Start happens once per mount when the Manager observes
// the config. Stop runs on graceful shutdown or when the operator
// removes the mount. Path is stable across the mount's lifetime —
// if the backing path ever needs to change the mount must be
// Stop'd and re-Start'd with a new config.
type Mount interface {
	Label() string
	Backend() string // "local" | "nfs" | "rclone"
	Path() string    // absolute filesystem path that backs this label
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Healthy() bool
}

// MountInfo is the lock-free snapshot the manager returns from List.
// Intended for display + audit; callers shouldn't mutate the backing
// Mount through this.
type MountInfo struct {
	Label   string
	Backend string
	Path    string
	Healthy bool
}

// Manager owns the set of active mounts on this node. Thread-safe
// for concurrent Resolve / List / Add / Remove calls. The Raft
// integration is a separate layer (see Phase 9c) that calls Register
// / Unregister on behalf of replicated config changes.
type Manager struct {
	mu     sync.RWMutex
	mounts map[string]Mount
}

// NewManager returns an empty manager. Mounts are registered
// through Register / Unregister; no start-on-boot logic here
// because the Raft config observer owns ordering.
func NewManager() *Manager {
	return &Manager{mounts: make(map[string]Mount)}
}

// Register adds a mount and starts it. Replaces any existing mount
// with the same label, stopping the old one first. Returns the
// concrete start error so callers can surface it to the operator.
func (m *Manager) Register(ctx context.Context, mount Mount) error {
	if mount == nil {
		return errors.New("storage: cannot register nil mount")
	}
	label := mount.Label()
	if label == "" {
		return errors.New("storage: mount label is required")
	}

	m.mu.Lock()
	old, exists := m.mounts[label]
	m.mounts[label] = mount
	m.mu.Unlock()

	if exists {
		_ = old.Stop(ctx)
	}
	if err := mount.Start(ctx); err != nil {
		m.mu.Lock()
		delete(m.mounts, label)
		m.mu.Unlock()
		return fmt.Errorf("storage: start %q: %w", label, err)
	}
	return nil
}

// Unregister stops the mount and removes it. No-op when the label
// is unknown — idempotent to keep Raft-observed deletes tolerant
// of re-deliveries.
func (m *Manager) Unregister(ctx context.Context, label string) error {
	m.mu.Lock()
	mount, exists := m.mounts[label]
	if exists {
		delete(m.mounts, label)
	}
	m.mu.Unlock()
	if !exists {
		return nil
	}
	return mount.Stop(ctx)
}

// Resolve returns the absolute filesystem path backing the given
// label, or ErrNotFound when the label isn't registered.
func (m *Manager) Resolve(label string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mount, ok := m.mounts[label]
	if !ok {
		return "", ErrNotFound
	}
	return mount.Path(), nil
}

// List returns a sorted-by-label snapshot of all active mounts.
func (m *Manager) List() []MountInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]MountInfo, 0, len(m.mounts))
	for _, mount := range m.mounts {
		out = append(out, MountInfo{
			Label:   mount.Label(),
			Backend: mount.Backend(),
			Path:    mount.Path(),
			Healthy: mount.Healthy(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// StopAll stops every mount. Called from node.Shutdown. Best-effort
// — a single mount's Stop error is logged by the caller and
// doesn't abort the sweep.
func (m *Manager) StopAll(ctx context.Context) []error {
	m.mu.Lock()
	mounts := make([]Mount, 0, len(m.mounts))
	for _, mt := range m.mounts {
		mounts = append(mounts, mt)
	}
	m.mounts = make(map[string]Mount)
	m.mu.Unlock()

	var errs []error
	for _, mt := range mounts {
		if err := mt.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("storage: stop %q: %w", mt.Label(), err))
		}
	}
	return errs
}
