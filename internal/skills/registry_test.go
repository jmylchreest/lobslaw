package skills

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func stubSkill(name, version, dir string) *Skill {
	return &Skill{
		Manifest:    Manifest{Name: name, Version: version, Runtime: RuntimePython, Handler: "h.py"},
		ManifestDir: dir,
		HandlerPath: dir + "/h.py",
		SHA256:      "stub",
	}
}

func TestRegistryPutAndGet(t *testing.T) {
	t.Parallel()
	r := NewRegistry(nil)
	s := stubSkill("greeter", "1.0.0", "/tmp/a")
	r.Put(s)

	got, err := r.Get("greeter")
	if err != nil {
		t.Fatal(err)
	}
	if got.Manifest.Version != "1.0.0" {
		t.Errorf("version: %q", got.Manifest.Version)
	}
}

func TestRegistryGetUnknown(t *testing.T) {
	t.Parallel()
	r := NewRegistry(nil)
	_, err := r.Get("nope")
	if !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("want ErrSkillNotFound; got %v", err)
	}
}

// TestRegistryHighestVersionWins — two mounts expose the same
// skill name. The registry picks the higher semver as the winner.
func TestRegistryHighestVersionWins(t *testing.T) {
	t.Parallel()
	r := NewRegistry(nil)
	r.Put(stubSkill("greeter", "1.0.0", "/mnt/older"))
	r.Put(stubSkill("greeter", "2.1.0", "/mnt/newer"))

	got, _ := r.Get("greeter")
	if got.ManifestDir != "/mnt/newer" {
		t.Errorf("winner: %q", got.ManifestDir)
	}
}

// TestRegistryRemoveFallsBackToOtherCandidate — removing the winner
// must re-elect the runner-up, not drop the name entirely.
func TestRegistryRemoveFallsBackToOtherCandidate(t *testing.T) {
	t.Parallel()
	r := NewRegistry(nil)
	r.Put(stubSkill("greeter", "1.0.0", "/mnt/a"))
	r.Put(stubSkill("greeter", "2.0.0", "/mnt/b"))

	r.Remove("/mnt/b")
	got, err := r.Get("greeter")
	if err != nil {
		t.Fatalf("fallback lost: %v", err)
	}
	if got.ManifestDir != "/mnt/a" {
		t.Errorf("fallback winner: %q", got.ManifestDir)
	}
}

// TestRegistryRemoveLastCandidateDropsName — with no candidates
// left, the name is unregistered.
func TestRegistryRemoveLastCandidateDropsName(t *testing.T) {
	t.Parallel()
	r := NewRegistry(nil)
	r.Put(stubSkill("greeter", "1.0.0", "/mnt/a"))
	r.Remove("/mnt/a")
	if _, err := r.Get("greeter"); !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("name should be unregistered; got %v", err)
	}
}

// TestRegistryPutReplaceSameDir — re-Put'ing the same ManifestDir
// with a bumped version updates the winner in place.
func TestRegistryPutReplaceSameDir(t *testing.T) {
	t.Parallel()
	r := NewRegistry(nil)
	r.Put(stubSkill("greeter", "1.0.0", "/mnt/a"))
	r.Put(stubSkill("greeter", "1.2.0", "/mnt/a"))

	got, _ := r.Get("greeter")
	if got.Manifest.Version != "1.2.0" {
		t.Errorf("version after update: %q", got.Manifest.Version)
	}
}

func TestRegistryListSorted(t *testing.T) {
	t.Parallel()
	r := NewRegistry(nil)
	r.Put(stubSkill("charlie", "1.0.0", "/mnt/c"))
	r.Put(stubSkill("alpha", "1.0.0", "/mnt/a"))
	r.Put(stubSkill("bravo", "1.0.0", "/mnt/b"))

	list := r.List()
	if len(list) != 3 {
		t.Fatalf("len=%d", len(list))
	}
	want := []string{"alpha", "bravo", "charlie"}
	for i, w := range want {
		if list[i].Name() != w {
			t.Errorf("order[%d]: %q want %q", i, list[i].Name(), w)
		}
	}
}

// TestRegistryScan walks a dir with two skill subdirs + one that
// isn't a skill and registers only the two valid skills.
func TestRegistryScan(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	sk1 := filepath.Join(root, "greeter")
	_ = os.Mkdir(sk1, 0o755)
	_ = os.WriteFile(filepath.Join(sk1, "h.py"), []byte("print()"), 0o755)
	_ = os.WriteFile(filepath.Join(sk1, "manifest.yaml"), []byte(`name: greeter
version: 1.0.0
runtime: python
handler: h.py`), 0o644)

	sk2 := filepath.Join(root, "summariser")
	_ = os.Mkdir(sk2, 0o755)
	_ = os.WriteFile(filepath.Join(sk2, "h.sh"), []byte("echo"), 0o755)
	_ = os.WriteFile(filepath.Join(sk2, "manifest.yaml"), []byte(`name: summariser
version: 2.0.0
runtime: bash
handler: h.sh`), 0o644)

	// Directory without a manifest — must be quietly skipped.
	notASkill := filepath.Join(root, "random-docs")
	_ = os.Mkdir(notASkill, 0o755)
	_ = os.WriteFile(filepath.Join(notASkill, "README.md"), []byte("hi"), 0o644)

	r := NewRegistry(nil)
	errs := r.Scan(root)
	if len(errs) != 0 {
		t.Errorf("unexpected parse errors: %v", errs)
	}
	if len(r.List()) != 2 {
		t.Errorf("expected 2 skills; got %d", len(r.List()))
	}
}

// TestRegistryScanSurfaceIndividualParseErrors — a malformed
// manifest surfaces as an error; other skills in the same root
// still register.
func TestRegistryScanSurfaceIndividualParseErrors(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	bad := filepath.Join(root, "broken")
	_ = os.Mkdir(bad, 0o755)
	_ = os.WriteFile(filepath.Join(bad, "manifest.yaml"), []byte("not yaml: : : :"), 0o644)

	good := filepath.Join(root, "good")
	_ = os.Mkdir(good, 0o755)
	_ = os.WriteFile(filepath.Join(good, "h.sh"), []byte("echo"), 0o755)
	_ = os.WriteFile(filepath.Join(good, "manifest.yaml"), []byte(`name: good
version: 1.0.0
runtime: bash
handler: h.sh`), 0o644)

	r := NewRegistry(nil)
	errs := r.Scan(root)
	if len(errs) == 0 {
		t.Error("malformed manifest should produce an error")
	}
	if _, err := r.Get("good"); err != nil {
		t.Errorf("sibling skill should still register; got %v", err)
	}
	if _, err := r.Get("broken"); err == nil {
		t.Error("broken skill should NOT register")
	}
}
