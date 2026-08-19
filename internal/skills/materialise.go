package skills

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Materialising the store onto disk.
//
// The store is the authority for what the agent has taught itself;
// the filesystem is the execution substrate. They are separate on
// purpose, and this is the one-way bridge between them.
//
// One way, and only one. The cache is written by the materialiser and
// by nothing else, so an artefact edited on disk is not an edit — it
// is a change that the next materialisation silently reverts. That is
// the property being bought: rm -rf the cache, restart, and you are
// exactly where you were. If that ever stops being true, the store has
// stopped being the authority and something on disk has quietly become
// one.

// Artefact is one ACTIVE record, flattened to what a skill needs.
//
// Deliberately not the proto: this package must not learn the memory
// package's schema to write a directory, and the caller — which
// already knows both — is the right place for the translation.
type Artefact struct {
	Name        string
	Description string
	Body        string
	Version     uint32
	// Files maps a relative path to its content. Written alongside
	// SKILL.md and declared as references, so the index can say what
	// is available without anything reading it.
	Files map[string]string
}

// BodyFile is where a prose skill's body lands. Named for what a
// reader will expect rather than for what the store calls it.
const BodyFile = "SKILL.md"

// manifestFile is the name Parse looks for. Named once here rather
// than repeated as a literal, because the materialiser and the parser
// disagreeing about it would produce a cache that writes fine and
// loads nothing.
const manifestFile = "manifest.yaml"

// Cache subtrees, one per provenance.
//
// Namespaced rather than flat, because the two are scanned by
// different code with different authority: everything under Agent is
// tagged TierAgent and passed through the capability floor, while
// everything under Imported has its signature verified and its tier
// derived from that. A single directory holding both would make the
// tier a property of who happened to scan it first.
//
// One root above them, so `rm -rf` the cache is still one command.
const (
	AgentSubtree    = "agent"
	ImportedSubtree = "imported"
)

// Materialiser writes ACTIVE artefacts into a per-node cache.
type Materialiser struct {
	root string
	log  *slog.Logger
}

// NewMaterialiser roots a cache at dir. The directory is created on
// first use, not here — a node with self-learning enabled and nothing
// learned yet should not leave an empty directory behind as evidence
// of a feature it never used.
func NewMaterialiser(dir string, log *slog.Logger) (*Materialiser, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("skills: materialiser root %q must be absolute", dir)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Materialiser{root: filepath.Clean(dir), log: log}, nil
}

// Root is the cache directory.
func (m *Materialiser) Root() string { return m.root }

// AgentRoot and ImportedRoot are what a scan points at. Exposed so a
// caller never reconstructs the path and gets it subtly wrong — and
// so pointing the agent scanner at the imported subtree is a
// misspelling rather than a plausible line of code.
func (m *Materialiser) AgentRoot() string    { return filepath.Join(m.root, AgentSubtree) }
func (m *Materialiser) ImportedRoot() string { return filepath.Join(m.root, ImportedSubtree) }

// Result reports what one pass did.
type Result struct {
	Written []string // skill dirs written or refreshed
	Pruned  []string // dirs removed because the record no longer is ACTIVE
	// Refused names artefacts that could not be written, by name and
	// reason. Not an error: one malformed artefact must not stop the
	// rest, and the operator needs to know which one.
	Refused map[string]string
}

// Materialise makes the cache match the given set of ACTIVE artefacts
// and returns what changed.
//
// Convergent, not incremental. It writes what should be there and
// removes what should not, from the full set each time, so a cache
// that drifted — a half-written directory from a crash, a stale
// version from a rollback, a file somebody edited — is corrected by
// the next pass rather than accumulating. An incremental version would
// need to be told what changed, and the thing that would have to tell
// it is the store, which does not know what any particular node has on
// disk.
func (m *Materialiser) Materialise(artefacts []Artefact) (Result, error) {
	res := Result{Refused: map[string]string{}}

	keep := make(map[string]string, len(artefacts))
	for _, a := range artefacts {
		if reason := refuseArtefact(a); reason != "" {
			res.Refused[a.Name] = reason
			continue
		}
		keep[a.Name] = versionString(a.Version)
	}

	for _, a := range artefacts {
		if _, ok := keep[a.Name]; !ok {
			continue
		}
		dir, err := m.writeSkill(a)
		if err != nil {
			// Recorded, not returned. One artefact that will not write
			// — a permission problem on its directory, say — must not
			// take the rest of the library down with it.
			res.Refused[a.Name] = err.Error()
			continue
		}
		res.Written = append(res.Written, dir)
	}

	pruned, err := m.prune(m.AgentRoot(), keep)
	if err != nil {
		return res, err
	}
	res.Pruned = pruned
	sort.Strings(res.Written)
	sort.Strings(res.Pruned)
	return res, nil
}

// refuseArtefact returns why an artefact cannot become a directory, or
// "" if it can.
//
// Checked BEFORE any path is built from the name. A name containing a
// separator is a traversal, and the moment it reaches filepath.Join it
// is one — so it is refused here rather than validated after the fact
// by the parse that reads the result back.
func refuseArtefact(a Artefact) string {
	name := strings.TrimSpace(a.Name)
	switch {
	case name == "":
		return "the artefact has no name"
	case name != a.Name:
		return fmt.Sprintf("name %q has leading or trailing whitespace", a.Name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Sprintf("name %q contains a path separator", name)
	case name == "." || name == "..":
		return fmt.Sprintf("name %q is not a directory name", name)
	case strings.HasPrefix(name, "."):
		return fmt.Sprintf("name %q starts with a dot, which the scan skips", name)
	case strings.TrimSpace(a.Body) == "":
		return "the artefact has an empty body, so there is nothing to teach"
	}
	for path := range a.Files {
		if reason := refuseFilePath(path); reason != "" {
			return reason
		}
	}
	return ""
}

func refuseFilePath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "a bundled file has an empty path"
	}
	cleaned := filepath.Clean(path)
	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Sprintf("bundled file %q is outside the skill directory", path)
	}
	if cleaned == BodyFile || cleaned == manifestFile || cleaned == manifestFile+".sig" {
		return fmt.Sprintf("bundled file %q would overwrite a file the materialiser owns", path)
	}
	return ""
}

// versionString renders a record version as semver.
//
// 0.0.N, so an agent-authored skill is visibly pre-1.0 in every
// listing, and so record order is version order within the tier —
// which is what the registry compares once tier has already decided
// that an operator skill of the same name wins outright.
func versionString(v uint32) string { return fmt.Sprintf("0.0.%d", v) }

// writeSkill writes one artefact to <root>/<name>/<version>/ and
// returns the directory.
//
// Staged and renamed rather than written in place. A scan can run at
// any moment — the watcher fires on the writes this makes — and a
// directory that appears half-built parses as a broken skill, logs an
// error, and is then never retried because the next pass sees the
// content it expected. The rename is the one operation that cannot be
// observed partway through.
func (m *Materialiser) writeSkill(a Artefact) (string, error) {
	version := versionString(a.Version)
	manifestYAML, err := renderManifest(a, version)
	if err != nil {
		return "", err
	}
	files := map[string][]byte{
		BodyFile:     []byte(a.Body),
		manifestFile: manifestYAML,
	}
	for path, content := range a.Files {
		files[filepath.ToSlash(filepath.Clean(path))] = []byte(content)
	}
	return m.writeVersion(m.AgentRoot(), a.Name, version, files)
}

// writeVersion writes one version directory atomically.
//
// Staged and renamed rather than written in place. A scan can run at
// any moment — the watcher fires on the writes this makes — and a
// directory that appears half-built parses as a broken skill, logs an
// error, and is then never retried because the next pass sees the
// content it expected. The rename is the one operation that cannot be
// observed partway through.
func (m *Materialiser) writeVersion(root, name, version string, files map[string][]byte) (string, error) {
	final := filepath.Join(root, name, version)
	if dirMatches(final, files) {
		return final, nil
	}

	nameDir := filepath.Join(root, name)
	if err := os.MkdirAll(nameDir, 0o700); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(nameDir, ".staging-")
	if err != nil {
		return "", err
	}
	// Removed on every path that does not rename it away. A staging
	// directory left behind after a failure is not inert: it is a
	// dotfile the scan skips, so it would accumulate silently.
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(staging)
		}
	}()

	for rel, content := range files {
		dest := filepath.Join(staging, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(dest, content, 0o600); err != nil {
			return "", err
		}
	}

	// The previous contents of THIS version directory. Removed first
	// because rename onto a non-empty directory fails.
	if err := os.RemoveAll(final); err != nil {
		return "", err
	}
	if err := os.Rename(staging, final); err != nil {
		return "", err
	}
	committed = true
	return final, nil
}

// renderManifest produces the manifest bytes for an artefact.
//
// Deterministic: references are sorted, so the bytes are a function of
// the record and nothing else. Go's map iteration order would
// otherwise make every pass produce a different file, defeating
// upToDate and rewriting the whole cache once a minute forever.
func renderManifest(a Artefact, version string) ([]byte, error) {
	// The body is a reference too, and named first. It is what the
	// index points at, and a skill whose body is not listed among its
	// files reads as a skill with no content.
	refs := make([]Reference, 0, len(a.Files)+1)
	for path := range a.Files {
		refs = append(refs, Reference{Path: filepath.ToSlash(filepath.Clean(path))})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Path < refs[j].Path })
	refs = append([]Reference{{Path: BodyFile}}, refs...)

	return yaml.Marshal(Manifest{
		Name:        a.Name,
		Version:     version,
		Description: indexDescription(a.Description),
		Runtime:     RuntimeProse,
		// Body as well as a reference. Listing it only under
		// references made skill_view answer "this skill ships no
		// instructions; its description in the index is all there is"
		// for a skill whose instructions were sitting in the very
		// directory it had just read — the body was reachable, but
		// only by asking for the file by name, which requires already
		// knowing it is there.
		Body:       BodyFile,
		References: refs,
	})
}

// dirMatches reports whether the directory already holds exactly these
// files, byte for byte.
//
// Compared by CONTENT rather than by version alone. A version match
// would be enough if the cache were only ever written here — which is
// the invariant, and comparing content is what makes a violation of it
// self-correcting rather than permanent.
//
// Anything unreadable counts as a mismatch. There is no failure worth
// reporting: the answer to "I cannot tell" and the answer to "no" are
// the same action.
func dirMatches(dir string, files map[string][]byte) bool {
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) //nolint:gosec // rel validated by the caller
		if err != nil || string(got) != string(want) {
			return false
		}
	}
	// A file present that the record does not name is drift too — a
	// reference removed upstream would otherwise stay readable forever,
	// because nothing comparing only the files it expects can notice
	// one it does not.
	return countFiles(dir) == len(files)
}

// countFiles counts regular files under dir, recursively.
func countFiles(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

// prune removes everything in the cache that keep does not name.
//
// Removal is what makes the store the authority in both directions.
// Without it, archiving an artefact would leave it loaded on every
// node that had ever seen it ACTIVE, and "forget what you taught
// yourself" would be true of the store and false of the thing actually
// in the prompt.
func (m *Materialiser) prune(root string, keep map[string]string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var pruned []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		nameDir := filepath.Join(root, e.Name())
		version, wanted := keep[e.Name()]
		if !wanted {
			if err := os.RemoveAll(nameDir); err != nil {
				return pruned, err
			}
			pruned = append(pruned, nameDir)
			continue
		}
		versions, err := os.ReadDir(nameDir)
		if err != nil {
			return pruned, err
		}
		for _, v := range versions {
			if v.Name() == version {
				continue
			}
			old := filepath.Join(nameDir, v.Name())
			if err := os.RemoveAll(old); err != nil {
				return pruned, err
			}
			pruned = append(pruned, old)
		}
	}
	return pruned, nil
}

// indexDescription fits a description into the one line the index
// renders.
//
// Truncated rather than refused. The limit exists because the
// description is in the system prompt on every turn, and an artefact
// that overran it by a word would otherwise fail to parse and vanish
// entirely — a skill with a clipped summary is a great deal more
// useful than a skill that is not there. The store is the place to
// reject an over-long description, at the moment its author could fix
// it; by the time it reaches here, nobody is watching.
func indexDescription(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= MaxDescriptionChars {
		return s
	}
	return strings.TrimSpace(string(runes[:MaxDescriptionChars-1])) + "…"
}

// --- imported skills -------------------------------------------------

// StoredSkill is one record from the cluster store, flattened to what
// a directory needs.
//
// The manifest arrives as BYTES, not as a parsed Manifest, and that is
// the whole reason the store keeps it verbatim: a signature is over
// the exact file, and anything that re-encodes on the way to disk
// breaks verification for a skill that genuinely was signed.
type StoredSkill struct {
	Name    string
	Version string
	// ManifestYAML is written unchanged. Never re-rendered.
	ManifestYAML []byte
	// ManifestSig is the detached signature, empty when unsigned.
	ManifestSig []byte
	Files       map[string][]byte
}

// MaterialiseStored makes the imported subtree match the store.
//
// Convergent like its agent-side counterpart, and separate from it
// because the two have different authority. Everything written here is
// scanned with signature verification and gets its tier from the
// result; everything written there is tagged TierAgent and passed
// through the capability floor. Sharing one directory would make the
// tier depend on which scanner reached it first.
func (m *Materialiser) MaterialiseStored(skills []StoredSkill) (Result, error) {
	res := Result{Refused: map[string]string{}}

	keep := make(map[string]string, len(skills))
	for _, sk := range skills {
		if reason := refuseStored(sk); reason != "" {
			res.Refused[sk.Name] = reason
			continue
		}
		keep[sk.Name] = sk.Version
	}

	for _, sk := range skills {
		if _, ok := keep[sk.Name]; !ok {
			continue
		}
		files := map[string][]byte{manifestFile: sk.ManifestYAML}
		if len(sk.ManifestSig) > 0 {
			// Written only when there is one. An empty .sig file makes
			// an unsigned skill look signed, and it then fails
			// verification rather than being correctly treated as
			// unsigned.
			files[signatureFile] = sk.ManifestSig
		}
		for rel, content := range sk.Files {
			files[filepath.ToSlash(filepath.Clean(rel))] = content
		}
		dir, err := m.writeVersion(m.ImportedRoot(), sk.Name, sk.Version, files)
		if err != nil {
			res.Refused[sk.Name] = err.Error()
			continue
		}
		res.Written = append(res.Written, dir)
	}

	pruned, err := m.prune(m.ImportedRoot(), keep)
	if err != nil {
		return res, err
	}
	res.Pruned = pruned
	sort.Strings(res.Written)
	sort.Strings(res.Pruned)
	return res, nil
}

// signatureFile sits beside the manifest, and the name must match what
// the parser looks for or a signed skill materialises as an unsigned
// one.
const signatureFile = manifestFile + ".sig"

// refuseStored returns why a stored record cannot become a directory.
//
// Checked BEFORE any path is built from the name or version — both
// become path segments here, and a separator in either is a traversal
// the moment it reaches filepath.Join. The version is checked too,
// which the agent side does not need: there the version is generated
// from a counter, while here it comes from a manifest somebody else
// wrote.
func refuseStored(sk StoredSkill) string {
	if reason := refuseSegment("name", sk.Name); reason != "" {
		return reason
	}

	if reason := refuseSegment("version", sk.Version); reason != "" {
		return reason
	}
	if len(sk.ManifestYAML) == 0 {
		return "the record has no manifest"
	}
	for path := range sk.Files {
		if reason := refuseFilePath(path); reason != "" {
			return reason
		}
	}
	return ""
}

func refuseSegment(what, v string) string {
	trimmed := strings.TrimSpace(v)
	switch {
	case trimmed == "":
		return "the record has no " + what
	case trimmed != v:
		return fmt.Sprintf("%s %q has leading or trailing whitespace", what, v)
	case strings.ContainsAny(trimmed, `/\`):
		return fmt.Sprintf("%s %q contains a path separator", what, trimmed)
	case trimmed == "." || trimmed == "..":
		return fmt.Sprintf("%s %q is not a directory name", what, trimmed)
	case strings.HasPrefix(trimmed, "."):
		return fmt.Sprintf("%s %q starts with a dot, which the scan skips", what, trimmed)
	}
	return ""
}
