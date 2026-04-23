// Package local implements the "local:" storage backend — a label
// pointing at an existing host directory. No namespace manipulation,
// no subprocess: the mount's Path is simply the configured source
// directory after resolving symlinks.
package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Mount is the local-backend implementation of storage.Mount.
// Start verifies the source exists + is a directory; Stop is a
// no-op (we didn't mount anything, just recorded a path).
type Mount struct {
	label   string
	source  string
	healthy bool
}

// Config is the per-mount operator-supplied shape. Kept narrow —
// future fields (e.g. a required/optional flag for operator gating)
// land here rather than bleeding into Mount itself.
type Config struct {
	Label  string
	Source string
}

// New constructs a local mount. Validates the config shape but does
// not touch the filesystem; Start performs the real-path check.
// Returns an error on missing label or empty source so misconfigs
// fail fast rather than at mount-time.
func New(cfg Config) (*Mount, error) {
	if cfg.Label == "" {
		return nil, errors.New("local: Label is required")
	}
	if cfg.Source == "" {
		return nil, errors.New("local: Source is required")
	}
	return &Mount{label: cfg.Label, source: cfg.Source}, nil
}

// Label returns the operator-chosen identifier.
func (m *Mount) Label() string { return m.label }

// Backend returns "local". Matches the StorageMount proto's backend
// string so storage-service list responses read uniformly.
func (m *Mount) Backend() string { return "local" }

// Path returns the resolved absolute path. Empty until Start has
// run successfully. Readers holding this path must treat it as
// immutable for the mount's lifetime — a Stop+new-Start with a
// different source produces a new Mount with a new Path.
func (m *Mount) Path() string { return m.source }

// Healthy reflects the last Start/Stop result. A failed Start or
// a manual Stop flips this to false.
func (m *Mount) Healthy() bool { return m.healthy }

// Start verifies the configured source exists and is a directory.
// Resolves symlinks so Path is stable even if the operator
// subsequently swaps the symlink target. Absolute paths only —
// relative paths fail so a restart from a different cwd can't
// silently retarget the mount.
func (m *Mount) Start(_ context.Context) error {
	if !filepath.IsAbs(m.source) {
		return fmt.Errorf("local: source %q must be an absolute path", m.source)
	}
	resolved, err := filepath.EvalSymlinks(m.source)
	if err != nil {
		return fmt.Errorf("local: resolve %q: %w", m.source, err)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("local: stat %q: %w", resolved, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("local: source %q is not a directory", resolved)
	}
	m.source = resolved
	m.healthy = true
	return nil
}

// Stop is a no-op for local mounts; the source directory continues
// to exist on the host unchanged. Sets healthy=false so subsequent
// Resolve callers see the mount is retired.
func (m *Mount) Stop(_ context.Context) error {
	m.healthy = false
	return nil
}
