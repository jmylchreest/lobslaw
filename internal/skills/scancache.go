package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Re-parsing a skill that has not changed.
//
// Both scans walk <root>/<name>/<version>/ and parse every directory
// they find, every time they run — and they run on a ticker whose real
// job is to poll a store, not to notice edits. A CPU profile of an idle
// node found its only activity here: ParseAgentSkill into
// promptguard.Scan into regexp backtracking, once a minute, to
// re-derive byte-for-byte what the registry already held.
//
// It is small at five skills. It is not small in the shape it grows:
// the work is per-skill, on a fixed interval, and the expensive part is
// a backtracking regexp over each skill's prose.
//
// So a directory is parsed when its contents differ from the last time
// it was parsed, and skipped otherwise.

// fingerprint hashes a skill directory's contents.
//
// Contents, not stat metadata. Size-and-mtime is the usual shortcut and
// it is cheaper, but it answers "has this probably changed", and the
// imported path derives a skill's TRUST TIER from what it parses. A
// same-size same-mtime edit is a strange thing to happen by accident
// and an obvious thing to arrange on purpose, so this reads the bytes.
//
// That read is not the cost being avoided. Skills are a manifest and
// some prose; hashing them is far cheaper than the YAML parse and the
// prompt-guard regexp pass that follow, which is what the profile
// actually blamed.
func fingerprint(dir string) (string, error) {
	type entry struct {
		rel  string
		hash [sha256.Size]byte
	}
	var entries []entry

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // path comes from walking dir
		if err != nil {
			return err
		}
		entries = append(entries, entry{rel: filepath.ToSlash(rel), hash: sha256.Sum256(b)})
		return nil
	})
	if err != nil {
		return "", err
	}

	// Sorted, because directory order is not guaranteed and a
	// fingerprint that changed when the filesystem felt like reordering
	// would cache nothing while looking like it cached everything.
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	sum := sha256.New()
	for _, e := range entries {
		sum.Write([]byte(e.rel))
		sum.Write([]byte{0}) // a separator no path can contain
		sum.Write(e.hash[:])
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// unchanged reports whether dir has been parsed before with exactly
// these contents, and records the fingerprint when it has not.
//
// A hash error is reported as CHANGED rather than propagated: the
// directory is about to be parsed anyway, and that parse produces a
// better error than "could not hash it". Failing towards doing the work
// keeps a broken cache slow rather than wrong.
func (r *Registry) unchanged(dir string) bool {
	fp, err := fingerprint(dir)
	if err != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.scanned == nil {
		r.scanned = map[string]string{}
	}
	if prev, ok := r.scanned[dir]; ok && prev == fp {
		return true
	}
	r.scanned[dir] = fp
	return false
}

// forgetScan drops a directory's fingerprint.
//
// Called from Remove, and the reason the cache cannot be a plain
// memo: a directory that is pruned and later reappears with the
// contents it had before would otherwise match a fingerprint recorded
// while it was registered, and be skipped — leaving a skill on disk
// that the registry does not know about.
func (r *Registry) forgetScan(dir string) {
	delete(r.scanned, dir)
}
