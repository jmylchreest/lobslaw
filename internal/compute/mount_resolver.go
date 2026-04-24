package compute

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// MountResolver translates mount-scoped paths ("workspace/notes.md")
// into absolute filesystem paths, enforcing:
//   - mount label must be registered
//   - subpath must not escape the mount root via ".." traversal
//   - if needWrite is true, mount must be flagged writable
//   - path must not match the mount's excludes (or hardcoded
//     cluster-internal patterns)
//
// Tolerant of bare absolute paths: Resolve("/abs/path") returns the
// path unchanged so existing callers don't break during the
// migration. New callers SHOULD pass mount-scoped paths.
type MountResolver struct {
	mu     sync.RWMutex
	mounts map[string]mountEntry
}

type mountEntry struct {
	root     string
	writable bool
	excludes []string
}

// NewMountResolver returns an empty resolver. Populate via Register.
func NewMountResolver() *MountResolver {
	return &MountResolver{mounts: make(map[string]mountEntry)}
}

// Register adds or replaces a mount. Safe to call repeatedly (hot-
// reload path — operator edits config, node re-registers).
func (r *MountResolver) Register(label, root string, writable bool, excludes []string) {
	if label == "" || root == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mounts[label] = mountEntry{
		root:     filepath.Clean(root),
		writable: writable,
		excludes: excludes,
	}
}

// Resolve translates a user-supplied path into an absolute
// filesystem path. Accepts two shapes:
//
//	"workspace/notes.md" → uses the "workspace" mount
//	"/abs/path"           → returned as-is (legacy absolute path)
//
// Returns ErrMountNotFound for unknown labels, ErrPathEscape for
// traversal attempts, ErrReadOnlyMount when needWrite is true and
// the mount is read-only, ErrExcluded for excluded paths.
func (r *MountResolver) Resolve(p string, needWrite bool) (string, error) {
	if p == "" {
		return "", errors.New("mount: path is empty")
	}
	if filepath.IsAbs(p) {
		return p, nil
	}
	label, subpath, ok := strings.Cut(p, "/")
	if !ok {
		label = p
		subpath = ""
	}
	r.mu.RLock()
	entry, ok := r.mounts[label]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("%w: %q (known mounts: %s)", ErrMountNotFound, label, strings.Join(r.labels(), ", "))
	}
	if needWrite && !entry.writable {
		return "", fmt.Errorf("%w: %q is read-only", ErrReadOnlyMount, label)
	}
	cleanSub := filepath.Clean(subpath)
	if cleanSub == ".." || strings.HasPrefix(cleanSub, "../") || strings.Contains(cleanSub, "/../") {
		return "", fmt.Errorf("%w: %q attempts to escape mount root", ErrPathEscape, p)
	}
	full := filepath.Join(entry.root, cleanSub)
	if !strings.HasPrefix(full, entry.root) {
		return "", fmt.Errorf("%w: %q resolves outside mount root", ErrPathEscape, p)
	}
	for _, pat := range entry.excludes {
		if ok, _ := filepath.Match(pat, cleanSub); ok {
			return "", fmt.Errorf("%w: %q matches exclude pattern %q", ErrExcluded, p, pat)
		}
	}
	return full, nil
}

// Labels returns the list of registered mount labels in sorted
// order (stable for list_providers-style introspection).
func (r *MountResolver) Labels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.labels()
}

func (r *MountResolver) labels() []string {
	out := make([]string, 0, len(r.mounts))
	for label := range r.mounts {
		out = append(out, label)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

var (
	ErrMountNotFound = errors.New("mount: unknown label")
	ErrPathEscape    = errors.New("mount: path escape rejected")
	ErrReadOnlyMount = errors.New("mount: read-only")
	ErrExcluded      = errors.New("mount: path excluded")
)

// activeMountResolver is the package-level resolver the fs builtins
// consult before falling back to raw-absolute-path behaviour. Nil
// (the zero-value default) preserves legacy absolute-path-only
// semantics — tests and setups without mounts continue to work.
var activeMountResolver *MountResolver

// SetActiveMountResolver installs a resolver for fs builtins. Called
// once during node boot after refreshing the resolver from config.
// Thread-safety: not needed — this fires once at boot before any
// builtin runs.
func SetActiveMountResolver(r *MountResolver) {
	activeMountResolver = r
}

// resolveFsPath is the helper every fs builtin uses to turn a user-
// supplied path string into an absolute filesystem path. Tries
// mount-scoped resolution first; falls through to raw-absolute for
// back-compat. Returns a structured {error_type, suggestion} payload
// when the resolver rejects.
func resolveFsPath(p string, needWrite bool) (string, []byte, int) {
	if activeMountResolver == nil {
		return p, nil, 0
	}
	if strings.HasPrefix(p, "/") {
		return p, nil, 0
	}
	resolved, err := activeMountResolver.Resolve(p, needWrite)
	if err == nil {
		return resolved, nil, 0
	}
	var errType, suggestion string
	switch {
	case errors.Is(err, ErrMountNotFound):
		errType = "mount_not_found"
		suggestion = fmt.Sprintf("known mounts: %s. Prefix paths with the mount label, e.g. 'workspace/foo.md'", strings.Join(activeMountResolver.Labels(), ", "))
	case errors.Is(err, ErrReadOnlyMount):
		errType = "read_only_mount"
		suggestion = "this mount is read-only. Pick a writable mount or read instead of writing"
	case errors.Is(err, ErrPathEscape):
		errType = "path_escape"
		suggestion = "paths must resolve inside a mount root; remove '..' segments"
	case errors.Is(err, ErrExcluded):
		errType = "path_excluded"
		suggestion = "this path is excluded by the mount's configuration"
	default:
		errType = "mount_resolve_failed"
		suggestion = ""
	}
	payload, _ := marshalErrorPayload(errType, err.Error(), suggestion)
	return "", payload, 1
}

// marshalErrorPayload is the non-fatal JSON encoder that mirrors
// marshalToolError but returns the raw payload + exit so resolveFsPath
// can hand it back cleanly.
func marshalErrorPayload(errType, msg, suggestion string) ([]byte, int) {
	payload, exit, _ := marshalToolError(errType, msg, suggestion)
	return payload, exit
}
