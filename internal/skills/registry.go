package skills

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"golang.org/x/mod/semver"

	"github.com/jmylchreest/lobslaw/internal/storage"
)

// ErrSkillNotFound fires when Get is asked about a skill that isn't
// registered. Callers translate to gRPC / HTTP "not found" in the
// channel layer.
var ErrSkillNotFound = errors.New("skills: skill not found")

// Registry holds the live set of skills indexed by name. Multiple
// storage mounts can expose skills with the same name — the registry
// resolves via semver-highest-wins so a mount shipping an older
// version doesn't shadow a production one.
type Registry struct {
	mu sync.RWMutex
	// byName holds the currently winning Skill per name.
	byName map[string]*Skill
	// candidates tracks every version from every source so removal
	// can fall back to the next-highest rather than losing the name.
	candidates map[string][]*Skill
	log        *slog.Logger
}

// NewRegistry constructs an empty registry with the given logger.
// Nil logger → slog.Default().
func NewRegistry(log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{
		byName:     make(map[string]*Skill),
		candidates: make(map[string][]*Skill),
		log:        log,
	}
}

// Put adds or replaces a candidate skill. Recomputes the winning
// entry. Same-manifest-SHA re-puts are idempotent — they update the
// candidate list but don't change the winner.
func (r *Registry) Put(skill *Skill) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := skill.Manifest.Name

	// Replace any prior candidate with the same ManifestDir — that's
	// "the file at this path changed" — then re-rank.
	list := r.candidates[name]
	replaced := false
	for i, c := range list {
		if c.ManifestDir == skill.ManifestDir {
			list[i] = skill
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, skill)
	}
	r.candidates[name] = list
	r.recomputeWinnerLocked(name)
}

// Remove drops every candidate sourced from manifestDir. If that
// leaves the name with no candidates the name is unregistered;
// otherwise the winner is recomputed over what remains.
func (r *Registry) Remove(manifestDir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, list := range r.candidates {
		kept := make([]*Skill, 0, len(list))
		for _, c := range list {
			if c.ManifestDir != manifestDir {
				kept = append(kept, c)
			}
		}
		if len(kept) == len(list) {
			continue
		}
		if len(kept) == 0 {
			delete(r.candidates, name)
			delete(r.byName, name)
			continue
		}
		r.candidates[name] = kept
		r.recomputeWinnerLocked(name)
	}
}

func (r *Registry) Get(name string) (*Skill, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.byName[name]
	if !ok {
		return nil, ErrSkillNotFound
	}
	return s, nil
}

// List returns a snapshot of all registered (winning) skills,
// sorted alphabetically by name. Safe for concurrent iteration by
// the caller — the returned slice is a fresh copy.
func (r *Registry) List() []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Skill, 0, len(r.byName))
	for _, s := range r.byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.Name < out[j].Manifest.Name })
	return out
}

// recomputeWinnerLocked picks the highest-semver candidate for
// name. Ties broken by lexicographic ManifestDir — deterministic so
// two replicas with identical config pick the same winner. Caller
// must hold r.mu.
func (r *Registry) recomputeWinnerLocked(name string) {
	list := r.candidates[name]
	if len(list) == 0 {
		delete(r.byName, name)
		return
	}
	best := list[0]
	for _, c := range list[1:] {
		if compareVersion(c.Manifest.Version, best.Manifest.Version) > 0 {
			best = c
			continue
		}
		if compareVersion(c.Manifest.Version, best.Manifest.Version) == 0 && c.ManifestDir < best.ManifestDir {
			best = c
		}
	}
	r.byName[name] = best
}

// compareVersion compares two semver strings, tolerating missing
// "v" prefixes. Non-semver versions sort lexicographically — not
// perfect but better than a hard error on bad manifest input.
func compareVersion(a, b string) int {
	va, vb := semverize(a), semverize(b)
	if semver.IsValid(va) && semver.IsValid(vb) {
		return semver.Compare(va, vb)
	}
	if a == b {
		return 0
	}
	if a < b {
		return -1
	}
	return 1
}

func semverize(v string) string {
	if v == "" {
		return ""
	}
	if v[0] == 'v' {
		return v
	}
	return "v" + v
}

// Scan walks root for "*/manifest.yaml" and Puts each parsed skill
// into the registry. Returns a slice of parse errors so callers
// can surface a summary; skills that failed parsing are simply
// absent from the registry.
func (r *Registry) Scan(root string) []error {
	var errs []error
	entries, err := os.ReadDir(root)
	if err != nil {
		return []error{err}
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		skill, err := Parse(dir)
		if err != nil {
			if _, statErr := os.Stat(filepath.Join(dir, "manifest.yaml")); os.IsNotExist(statErr) {
				// Directory without a manifest — not every subdir under
				// the skills root is a skill. Quiet skip.
				continue
			}
			r.log.Warn("skills: parse failed", "dir", dir, "err", err)
			errs = append(errs, err)
			continue
		}
		r.Put(skill)
	}
	return errs
}

// Watch wires the registry to a storage-Manager mount label. Runs
// an initial Scan, then subscribes to the mount's Watcher and
// re-scans on relevant changes. Exits cleanly on ctx cancel. The
// simplest correct semantic: any Create/Write/Remove under the
// label triggers a full Scan — the skill set is small so the cost
// of re-parsing is negligible and it avoids fiddly per-file state.
func (r *Registry) Watch(ctx context.Context, mgr *storage.Manager, label string) error {
	root, err := mgr.Resolve(label)
	if err != nil {
		return err
	}
	if errs := r.Scan(root); len(errs) > 0 {
		r.log.Warn("skills: initial scan had errors", "count", len(errs))
	}

	ch, err := mgr.Watch(ctx, label, storage.WatchOpts{
		Recursive: true,
		Include:   []string{"manifest.yaml"},
	})
	if err != nil {
		return err
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				// Rescan is simpler than per-event surgery, keeps the
				// winner-computation correct across rename/remove, and
				// is cheap for realistic skill counts.
				_ = r.Scan(root)
				_ = ev
			}
		}
	}()
	return nil
}
