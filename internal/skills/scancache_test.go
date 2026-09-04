package skills

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// The cache's contract, checked directly: a directory is "changed" the
// first time it is seen, unchanged while its contents hold still, and
// changed again the moment any byte of it differs.
func TestFingerprintCacheTracksContent(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	if _, err := m.Materialise([]Artefact{artefact("tidy", "body", 1)}); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(slog.New(slog.DiscardHandler))
	dir := filepath.Join(m.AgentRoot(), "tidy", "0.0.1")

	if r.unchanged(dir) {
		t.Fatal("a directory never seen before reported unchanged")
	}
	if !r.unchanged(dir) {
		t.Fatal("an untouched directory reported changed; nothing would ever be cached")
	}

	// An edit that keeps the file count and could keep the size.
	body := filepath.Join(dir, BodyFile)
	before, err := os.ReadFile(body)
	if err != nil {
		t.Fatal(err)
	}
	edited := append([]byte(nil), before...)
	edited[0] ^= 0xFF // same length, different content
	if err := os.WriteFile(body, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if r.unchanged(dir) {
		t.Error("a same-length content edit went unnoticed; the fingerprint is not reading the bytes")
	}

	// Adding a file is a change even though no existing file moved.
	if err := os.WriteFile(filepath.Join(dir, "extra.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r.unchanged(dir) {
		t.Error("an added file went unnoticed")
	}
}

// Remove has to drop the fingerprint, or a directory that is pruned and
// comes back with the contents it had before would match a hash
// recorded while it was registered — and be skipped, leaving a skill on
// disk that the registry does not know about.
func TestRemoveInvalidatesTheFingerprint(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	if _, err := m.Materialise([]Artefact{artefact("tidy", "body", 1)}); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(slog.New(slog.DiscardHandler))
	if errs := r.ScanAgent(m.AgentRoot()); len(errs) != 0 {
		t.Fatalf("scan: %v", errs)
	}
	if _, err := r.Get("tidy"); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(m.AgentRoot(), "tidy", "0.0.1")
	r.Remove(dir)
	if _, err := r.Get("tidy"); err == nil {
		t.Fatal("Remove did not unregister the skill")
	}

	// Same bytes on disk, so only a dropped fingerprint can bring it
	// back.
	if errs := r.ScanAgent(m.AgentRoot()); len(errs) != 0 {
		t.Fatalf("rescan: %v", errs)
	}
	if _, err := r.Get("tidy"); err != nil {
		t.Error("a removed-then-rescanned skill did not come back; its fingerprint outlived it")
	}
}

// Scanning repeatedly must not change what is registered — the cache is
// an optimisation, not a behaviour.
func TestRepeatedScansAreStable(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	if _, err := m.Materialise([]Artefact{
		artefact("tidy", "b", 1), artefact("summarise", "b", 2),
	}); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(slog.New(slog.DiscardHandler))
	for i := range 3 {
		if errs := r.ScanAgent(m.AgentRoot()); len(errs) != 0 {
			t.Fatalf("scan %d: %v", i, errs)
		}
		if got := len(r.List()); got != 2 {
			t.Fatalf("scan %d registered %d skills, want 2", i, got)
		}
	}
}

// An edit on disk still takes effect, which is the property the cache
// is most likely to break.
func TestEditedSkillIsRescanned(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	if _, err := m.Materialise([]Artefact{artefact("tidy", "original body", 1)}); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(slog.New(slog.DiscardHandler))
	if errs := r.ScanAgent(m.AgentRoot()); len(errs) != 0 {
		t.Fatalf("scan: %v", errs)
	}
	first, err := r.Get("tidy")
	if err != nil {
		t.Fatal(err)
	}
	before := first.BodySHA256

	dir := filepath.Join(m.AgentRoot(), "tidy", "0.0.1")
	if err := os.WriteFile(filepath.Join(dir, BodyFile), []byte("replaced body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if errs := r.ScanAgent(m.AgentRoot()); len(errs) != 0 {
		t.Fatalf("rescan: %v", errs)
	}
	after, err := r.Get("tidy")
	if err != nil {
		t.Fatal(err)
	}
	if after.BodySHA256 == before {
		t.Errorf("body digest is unchanged at %s; the edit was cached away", before)
	}
}
