package compute

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// MountMode is a unified read/write/exec triple. Replaces the old
// boolean writable flag because (a) Landlock distinguishes all three
// and (b) operators legitimately want "rx" for binary mounts that
// shell_command may run but the agent must not modify.
//
// Parsed from string forms: "r", "ro", "rw", "rx", "rwx", "" (= "r").
// Case-insensitive; characters may appear in any order.
type MountMode struct {
	Read, Write, Exec bool
}

// ParseMountMode accepts the string forms above. Empty string defaults
// to read-only (the safest choice for a misconfigured mount). Returns
// an error on unknown characters so a typo doesn't silently downgrade
// to no-access.
func ParseMountMode(s string) (MountMode, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return MountMode{Read: true}, nil
	}
	// Tolerate "ro" / "rw" / "rx" / "rwx" — common shorthand.
	if s == "ro" {
		return MountMode{Read: true}, nil
	}
	var m MountMode
	for _, r := range s {
		switch r {
		case 'r':
			m.Read = true
		case 'w':
			m.Write = true
		case 'x':
			m.Exec = true
		default:
			return MountMode{}, fmt.Errorf("mount mode: unknown character %q in %q", r, s)
		}
	}
	if !m.Read && (m.Write || m.Exec) {
		// Write-without-read or exec-without-read isn't meaningful
		// at the FS layer and Landlock requires Read for anything
		// useful. Coerce upward; saves operators from a footgun.
		m.Read = true
	}
	return m, nil
}

// String returns canonical "rwx" / "rw" / "rx" / "r" form.
func (m MountMode) String() string {
	var b strings.Builder
	if m.Read {
		b.WriteByte('r')
	}
	if m.Write {
		b.WriteByte('w')
	}
	if m.Exec {
		b.WriteByte('x')
	}
	if b.Len() == 0 {
		return "-"
	}
	return b.String()
}

// MountResolver translates mount-scoped paths ("workspace/notes.md")
// or absolute paths into validated absolute filesystem paths,
// enforcing:
//   - mount label must be registered (label-prefixed form), OR
//     absolute path must fall under a registered mount root
//   - subpath must not escape the mount root via ".." traversal
//   - mount mode must permit the requested access (read/write/exec)
//   - path must not match the mount's excludes (or hardcoded
//     cluster-internal patterns)
//
// Absolute paths that fall outside every registered mount are
// rejected — the agent has no business reading /etc/passwd just
// because it knows the path. This is the storage-isolation contract.
type MountResolver struct {
	mu     sync.RWMutex
	mounts map[string]mountEntry
}

type mountEntry struct {
	root     string
	mode     MountMode
	excludes []string
}

// NewMountResolver returns an empty resolver. Populate via Register.
func NewMountResolver() *MountResolver {
	return &MountResolver{mounts: make(map[string]mountEntry)}
}

// Register adds or replaces a mount. Safe to call repeatedly (hot-
// reload path — operator edits config, node re-registers).
func (r *MountResolver) Register(label, root string, mode MountMode, excludes []string) {
	if label == "" || root == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mounts[label] = mountEntry{
		root:     filepath.Clean(root),
		mode:     mode,
		excludes: excludes,
	}
}

// Resolve translates a user-supplied path into a validated absolute
// filesystem path. Accepts two shapes:
//
//	"workspace/notes.md"   → resolves under the "workspace" mount
//	"/abs/path"            → must fall inside SOME mount root
//
// need expresses what the caller wants to do with the path. If the
// mount's mode doesn't grant a requested bit, returns ErrModeDenied.
func (r *MountResolver) Resolve(p string, need MountMode) (string, error) {
	if p == "" {
		return "", errors.New("mount: path is empty")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	if filepath.IsAbs(p) {
		return r.resolveAbsoluteLocked(p, need)
	}
	return r.resolveLabelLocked(p, need)
}

// resolveAbsoluteLocked walks the registered mounts and finds the one
// whose root is a prefix of p. Caller holds r.mu (read).
func (r *MountResolver) resolveAbsoluteLocked(p string, need MountMode) (string, error) {
	clean := filepath.Clean(p)
	var (
		matchedLabel string
		matchedEntry mountEntry
		matchedSub   string
		bestLen      int
	)
	for label, entry := range r.mounts {
		root := entry.root
		if !strings.HasPrefix(clean, root) {
			continue
		}
		// Avoid /foo/bar matching mount /foo when /foobar is a sibling.
		if len(clean) > len(root) && clean[len(root)] != filepath.Separator {
			continue
		}
		if len(root) > bestLen {
			matchedLabel = label
			matchedEntry = entry
			bestLen = len(root)
			if len(clean) == len(root) {
				matchedSub = ""
			} else {
				matchedSub = clean[len(root)+1:]
			}
		}
	}
	if matchedLabel == "" {
		return "", fmt.Errorf("%w: %q is outside every mount root (known mounts: %s)",
			ErrOutsideMounts, p, strings.Join(r.labelsLocked(), ", "))
	}
	if err := checkMode(matchedLabel, matchedEntry.mode, need); err != nil {
		return "", err
	}
	if err := checkExcludes(p, matchedSub, matchedEntry.excludes); err != nil {
		return "", err
	}
	return clean, nil
}

// resolveLabelLocked handles the "label/subpath" shape.
func (r *MountResolver) resolveLabelLocked(p string, need MountMode) (string, error) {
	label, subpath, ok := strings.Cut(p, "/")
	if !ok {
		label = p
		subpath = ""
	}
	entry, ok := r.mounts[label]
	if !ok {
		return "", fmt.Errorf("%w: %q (known mounts: %s)", ErrMountNotFound, label, strings.Join(r.labelsLocked(), ", "))
	}
	if err := checkMode(label, entry.mode, need); err != nil {
		return "", err
	}
	cleanSub := filepath.Clean(subpath)
	if cleanSub == ".." || strings.HasPrefix(cleanSub, "../") || strings.Contains(cleanSub, "/../") {
		return "", fmt.Errorf("%w: %q attempts to escape mount root", ErrPathEscape, p)
	}
	full := filepath.Join(entry.root, cleanSub)
	if !strings.HasPrefix(full, entry.root) {
		return "", fmt.Errorf("%w: %q resolves outside mount root", ErrPathEscape, p)
	}
	if err := checkExcludes(p, cleanSub, entry.excludes); err != nil {
		return "", err
	}
	return full, nil
}

func checkMode(label string, have, need MountMode) error {
	if need.Write && !have.Write {
		return fmt.Errorf("%w: %q is %s, write requested", ErrModeDenied, label, have.String())
	}
	if need.Exec && !have.Exec {
		return fmt.Errorf("%w: %q is %s, exec requested", ErrModeDenied, label, have.String())
	}
	if need.Read && !have.Read {
		return fmt.Errorf("%w: %q is %s, read requested", ErrModeDenied, label, have.String())
	}
	return nil
}

func checkExcludes(displayPath, subpath string, excludes []string) error {
	if subpath == "" {
		return nil
	}
	for _, pat := range excludes {
		if ok, _ := filepath.Match(pat, subpath); ok {
			return fmt.Errorf("%w: %q matches exclude pattern %q", ErrExcluded, displayPath, pat)
		}
	}
	return nil
}

// Labels returns the list of registered mount labels in sorted order.
func (r *MountResolver) Labels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.labelsLocked()
}

// MountInfo is a snapshot of one registered mount. Used by callers
// that need the full picture (Landlock helper, debug_storage tool).
type MountInfo struct {
	Label    string
	Root     string
	Mode     MountMode
	Excludes []string
}

// Mounts returns a snapshot of every registered mount, sorted by
// label. Caller may keep the slice — it is a copy of the live state.
func (r *MountResolver) Mounts() []MountInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]MountInfo, 0, len(r.mounts))
	for label, entry := range r.mounts {
		ex := append([]string(nil), entry.excludes...)
		out = append(out, MountInfo{
			Label:    label,
			Root:     entry.root,
			Mode:     entry.mode,
			Excludes: ex,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

func (r *MountResolver) labelsLocked() []string {
	out := make([]string, 0, len(r.mounts))
	for label := range r.mounts {
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

var (
	ErrMountNotFound = errors.New("mount: unknown label")
	ErrOutsideMounts = errors.New("mount: absolute path outside every registered mount root")
	ErrPathEscape    = errors.New("mount: path escape rejected")
	ErrModeDenied    = errors.New("mount: access mode denied")
	ErrExcluded      = errors.New("mount: path excluded")

	// ErrReadOnlyMount kept for one release as an alias of ErrModeDenied
	// so callers that errors.Is on the old name keep working.
	ErrReadOnlyMount = ErrModeDenied
)

// activeMountResolver is the package-level resolver the fs builtins
// consult. Nil during tests that don't exercise mount enforcement;
// in that case resolveFsPath falls open with the raw path so simple
// unit tests don't need full storage wiring.
var activeMountResolver *MountResolver

// SetActiveMountResolver installs a resolver for fs builtins. Called
// once during node boot after refreshing the resolver from config.
func SetActiveMountResolver(r *MountResolver) {
	activeMountResolver = r
}

// resolveFsPath is the helper every fs builtin uses to turn a user-
// supplied path string into an absolute filesystem path. With a
// resolver installed every path — relative or absolute — must
// resolve through it. Without a resolver (test setup), passes the
// path through unchanged.
//
// needWrite=true demands a writable mount. Read access is implicit
// for any non-zero call. Exec checks are made via resolveFsPathExec
// instead, which only shell_command and the Landlock helper need.
func resolveFsPath(p string, needWrite bool) (string, []byte, int) {
	need := MountMode{Read: true, Write: needWrite}
	return resolveFsPathMode(p, need)
}

func resolveFsPathMode(p string, need MountMode) (string, []byte, int) {
	if activeMountResolver == nil {
		return p, nil, 0
	}
	resolved, err := activeMountResolver.Resolve(p, need)
	if err == nil {
		return resolved, nil, 0
	}
	var errType, suggestion string
	switch {
	case errors.Is(err, ErrMountNotFound):
		errType = "mount_not_found"
		suggestion = fmt.Sprintf("known mounts: %s. Prefix paths with the mount label, e.g. 'workspace/foo.md'", strings.Join(activeMountResolver.Labels(), ", "))
	case errors.Is(err, ErrOutsideMounts):
		errType = "outside_mounts"
		suggestion = fmt.Sprintf("absolute paths must fall under a registered mount root. Known mounts: %s. Prefer the label form like 'workspace/foo.md'", strings.Join(activeMountResolver.Labels(), ", "))
	case errors.Is(err, ErrModeDenied):
		errType = "mode_denied"
		suggestion = "the requested access (write/exec) is not granted by this mount's mode. Pick a mount with the right rwx bits or read instead of writing"
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
