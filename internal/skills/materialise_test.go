package skills

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The materialiser is the one-way bridge between the store (authority)
// and the filesystem (execution substrate). The property being bought
// is that rm -rf the cache and restart leaves you exactly where you
// were — so most of what follows is about convergence, not about
// writing files.

func materialiser(t *testing.T) *Materialiser {
	t.Helper()
	m, err := NewMaterialiser(t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func artefact(name, body string, version uint32) Artefact {
	return Artefact{Name: name, Description: "does a thing", Body: body, Version: version}
}

func TestMaterialiseWritesALoadableSkill(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	res, err := m.Materialise([]Artefact{artefact("tidy", "how to tidy", 3)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written) != 1 {
		t.Fatalf("written = %v", res.Written)
	}

	dir := filepath.Join(m.AgentRoot(), "tidy", "0.0.3")
	skill, err := ParseAgentSkill(dir)
	if err != nil {
		t.Fatalf("the materialised skill does not parse: %v", err)
	}
	if skill.Tier != TierAgent {
		t.Errorf("tier = %v, want agent", skill.Tier)
	}
	if skill.Manifest.Runtime != RuntimeProse {
		t.Errorf("runtime = %q, want prose", skill.Manifest.Runtime)
	}
	// A prose skill has no handler path, so nothing downstream can
	// mistake the manifest dir for a script.
	if skill.HandlerPath != "" {
		t.Errorf("HandlerPath = %q, want empty", skill.HandlerPath)
	}

	body, err := os.ReadFile(filepath.Join(dir, BodyFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "how to tidy" {
		t.Errorf("body = %q", body)
	}
	// The body has to be listed, or the index advertises a skill whose
	// content the model has no way to find.
	if paths := ReferencePaths(skill.Manifest.References); len(paths) == 0 || paths[0] != BodyFile {
		t.Errorf("references = %v; the body is not listed first", paths)
	}
}

func TestBundledFilesAreWrittenAndDeclared(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	a := artefact("tidy", "body", 1)
	a.Files = map[string]string{"references/api.md": "the api"}
	if _, err := m.Materialise([]Artefact{a}); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(m.AgentRoot(), "tidy", "0.0.1")
	got, err := os.ReadFile(filepath.Join(dir, "references", "api.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the api" {
		t.Errorf("reference content = %q", got)
	}
	skill, err := ParseAgentSkill(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range ReferencePaths(skill.Manifest.References) {
		if p == "references/api.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("the bundled file is on disk but undeclared: %v",
			ReferencePaths(skill.Manifest.References))
	}
}

// --- convergence ---------------------------------------------------

// An artefact that is no longer ACTIVE has to leave the cache, or
// "forget what you taught yourself" is true of the store and false of
// what is actually in the prompt.
func TestArchivedArtefactsArePruned(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	if _, err := m.Materialise([]Artefact{
		artefact("tidy", "b", 1), artefact("summarise", "b", 1),
	}); err != nil {
		t.Fatal(err)
	}

	res, err := m.Materialise([]Artefact{artefact("tidy", "b", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pruned) != 1 || !strings.HasSuffix(res.Pruned[0], "summarise") {
		t.Fatalf("pruned = %v, want the summarise directory", res.Pruned)
	}
	if _, err := os.Stat(filepath.Join(m.AgentRoot(), "summarise")); !os.IsNotExist(err) {
		t.Error("the archived artefact is still on disk")
	}
	if _, err := os.Stat(filepath.Join(m.AgentRoot(), "tidy", "0.0.1")); err != nil {
		t.Errorf("the active artefact was pruned too: %v", err)
	}
}

// A new version replaces the old directory rather than accumulating
// beside it. Two versions of one skill in the cache would both
// register, and the registry would pick by semver — correct, but it
// would mean a rollback took effect only after the higher version's
// directory happened to be cleaned up.
func TestASupersededVersionIsRemoved(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	if _, err := m.Materialise([]Artefact{artefact("tidy", "v1", 1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Materialise([]Artefact{artefact("tidy", "v2", 2)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.AgentRoot(), "tidy", "0.0.1")); !os.IsNotExist(err) {
		t.Error("the old version directory survived")
	}
	body, err := os.ReadFile(filepath.Join(m.AgentRoot(), "tidy", "0.0.2", BodyFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "v2" {
		t.Errorf("body = %q", body)
	}
}

// The cache is written by the materialiser and by nothing else — so an
// edit on disk is not an edit, it is drift, and the next pass corrects
// it. This is the test of whether the store is really the authority.
func TestAnEditedCacheIsCorrected(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	if _, err := m.Materialise([]Artefact{artefact("tidy", "the real body", 1)}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.AgentRoot(), "tidy", "0.0.1", BodyFile)
	if err := os.WriteFile(path, []byte("somebody edited this"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Materialise([]Artefact{artefact("tidy", "the real body", 1)}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the real body" {
		t.Errorf("body = %q; the edit survived a reconcile", body)
	}
}

// And deleting the cache entirely is complete recovery.
func TestDeletingTheCacheRecoversFully(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	arts := []Artefact{artefact("tidy", "b1", 1), artefact("summarise", "b2", 4)}
	if _, err := m.Materialise(arts); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(m.AgentRoot()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Materialise(arts); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tidy/0.0.1", "summarise/0.0.4"} {
		if _, err := ParseAgentSkill(filepath.Join(m.AgentRoot(), filepath.FromSlash(want))); err != nil {
			t.Errorf("%s did not come back: %v", want, err)
		}
	}
}

// An unchanged pass must not rewrite anything, or the ticker churns
// the disk and the watcher fires once a minute forever.
func TestAnUnchangedPassRewritesNothing(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	arts := []Artefact{artefact("tidy", "b", 1)}
	if _, err := m.Materialise(arts); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.AgentRoot(), "tidy", "0.0.1", BodyFile)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Materialise(arts); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Identity, not mtime. A rewrite goes through staging and a
	// rename, so it is a different file even when the clock has not
	// moved — which on a fast filesystem it will not have.
	if !os.SameFile(before, after) {
		t.Error("an unchanged artefact was rewritten")
	}
}

// No staging directory may survive a pass. They are dotfiles the scan
// skips, so leaked ones accumulate silently.
func TestNoStagingDirectoriesAreLeftBehind(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	if _, err := m.Materialise([]Artefact{artefact("tidy", "v1", 1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Materialise([]Artefact{artefact("tidy", "v2", 2)}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(m.AgentRoot(), "tidy"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".staging-") {
			t.Errorf("a staging directory survived: %s", e.Name())
		}
	}
}

// --- refusals ------------------------------------------------------

// Checked before any path is built from the name. A name with a
// separator IS a traversal the moment it reaches filepath.Join.
func TestNamesThatWouldEscapeTheCacheAreRefused(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"../escape", "a/b", `a\b`, "..", ".", ".hidden", ""} {
		m := materialiser(t)
		res, err := m.Materialise([]Artefact{artefact(name, "body", 1)})
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if _, refused := res.Refused[name]; !refused {
			t.Errorf("name %q was accepted", name)
		}
		if len(res.Written) != 0 {
			t.Errorf("name %q wrote %v", name, res.Written)
		}
	}
}

func TestBundledFilesCannotEscapeOrOverwrite(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"../out.md", "/etc/passwd", BodyFile, "manifest.yaml", ""} {
		m := materialiser(t)
		a := artefact("tidy", "body", 1)
		a.Files = map[string]string{path: "payload"}
		res, err := m.Materialise([]Artefact{a})
		if err != nil {
			t.Fatalf("%q: %v", path, err)
		}
		if _, refused := res.Refused["tidy"]; !refused {
			t.Errorf("bundled path %q was accepted", path)
		}
	}
}

// One bad artefact must not take the library down with it.
func TestOneRefusedArtefactDoesNotStopTheRest(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	res, err := m.Materialise([]Artefact{
		artefact("../escape", "body", 1),
		artefact("tidy", "body", 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written) != 1 {
		t.Fatalf("written = %v, want the good one", res.Written)
	}
	if len(res.Refused) != 1 {
		t.Errorf("refused = %v", res.Refused)
	}
}

func TestAnEmptyBodyIsRefused(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	res, err := m.Materialise([]Artefact{artefact("tidy", "   \n ", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if _, refused := res.Refused["tidy"]; !refused {
		t.Error("an artefact with no body was materialised")
	}
}

// --- the description -----------------------------------------------

// The description is in the system prompt on every turn, so it is
// capped at parse. Truncated rather than refused here: a skill with a
// clipped summary is far more useful than a skill that is not there,
// and by this point its author is long gone.
func TestAnOverlongDescriptionIsTruncatedNotRefused(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	a := artefact("tidy", "body", 1)
	a.Description = strings.Repeat("x", MaxDescriptionChars*2)
	if _, err := m.Materialise([]Artefact{a}); err != nil {
		t.Fatal(err)
	}
	skill, err := ParseAgentSkill(filepath.Join(m.AgentRoot(), "tidy", "0.0.1"))
	if err != nil {
		t.Fatalf("an overlong description made the skill unloadable: %v", err)
	}
	if n := len([]rune(skill.Manifest.Description)); n > MaxDescriptionChars {
		t.Errorf("description is %d chars", n)
	}
}

// A newline in a description would break the one-entry-per-line index,
// and parse refuses one — so it must not reach the manifest.
func TestAMultiLineDescriptionIsFlattened(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	a := artefact("tidy", "body", 1)
	a.Description = "first line\nsecond line"
	if _, err := m.Materialise([]Artefact{a}); err != nil {
		t.Fatal(err)
	}
	skill, err := ParseAgentSkill(filepath.Join(m.AgentRoot(), "tidy", "0.0.1"))
	if err != nil {
		t.Fatalf("a multi-line description made the skill unloadable: %v", err)
	}
	if strings.ContainsAny(skill.Manifest.Description, "\n\r") {
		t.Errorf("description = %q", skill.Manifest.Description)
	}
}

// --- registry integration ------------------------------------------

func TestScanAgentRegistersTheCache(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	if _, err := m.Materialise([]Artefact{
		artefact("tidy", "b", 1), artefact("summarise", "b", 2),
	}); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(slog.New(slog.DiscardHandler))
	if errs := r.ScanAgent(m.AgentRoot()); len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if len(r.List()) != 2 {
		t.Fatalf("registered %d skills", len(r.List()))
	}
	s, err := r.Get("tidy")
	if err != nil {
		t.Fatal(err)
	}
	if s.Tier != TierAgent {
		t.Errorf("tier = %v; a cache skill was registered above the agent tier", s.Tier)
	}
}

// An empty or absent cache is the normal state of a fresh node, not an
// error to log on every boot.
func TestScanAgentOnAMissingCacheIsSilent(t *testing.T) {
	t.Parallel()
	r := NewRegistry(slog.New(slog.DiscardHandler))
	if errs := r.ScanAgent(filepath.Join(t.TempDir(), "never-created")); len(errs) != 0 {
		t.Errorf("errs = %v", errs)
	}
}

// The whole point of tier-first precedence, exercised through the real
// materialised layout: an operator skill of the same name wins, and
// the agent cannot take it by choosing a higher version.
func TestAnOperatorSkillOutranksAMaterialisedOne(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	if _, err := m.Materialise([]Artefact{artefact("tidy", "the agent version", 99)}); err != nil {
		t.Fatal(err)
	}
	opDir := t.TempDir()
	writeHandler(t, opDir, "handler.py", "print('hi')")
	writeManifest(t, opDir, `
name: tidy
version: 0.0.1
runtime: python
handler: handler.py
`)
	operator, err := Parse(opDir)
	if err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(slog.New(slog.DiscardHandler))
	if errs := r.ScanAgent(m.AgentRoot()); len(errs) != 0 {
		t.Fatal(errs)
	}
	r.Put(operator)

	winner, err := r.Get("tidy")
	if err != nil {
		t.Fatal(err)
	}
	if winner.ManifestDir != opDir {
		t.Errorf("winner = %q; the agent's 0.0.99 beat the operator's 0.0.1", winner.ManifestDir)
	}
}

// skill_view's level-1 disclosure reads the manifest's body. Writing
// SKILL.md and listing it only under references made it answer "this
// skill ships no instructions" for a skill whose instructions were in
// the directory it had just read — reachable, but only by asking for
// the file by name, which requires already knowing it is there.
func TestTheManifestNamesTheBodyItJustWrote(t *testing.T) {
	t.Parallel()
	raw, err := renderManifest(Artefact{
		Name:        "Prepare Release Notes",
		Description: "write release notes",
		Body:        "1. search 2. write 3. summarise",
	}, "0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Body != BodyFile {
		t.Errorf("manifest body = %q, want %q — skill_view will report no instructions", m.Body, BodyFile)
	}
	// Still a reference too: a skill whose body is not among its
	// files reads as a skill with no content.
	var listed bool
	for _, r := range m.References {
		if r.Path == BodyFile {
			listed = true
		}
	}
	if !listed {
		t.Error("the body is no longer listed among the references")
	}
}
