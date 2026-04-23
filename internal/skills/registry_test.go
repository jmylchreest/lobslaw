package skills

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
)

// fakePolicySink records every SetPolicy call. Satisfies
// sandbox.PolicySink for Registry tests without pulling in the
// real compute.Registry (which would cycle the import graph).
type fakePolicySink struct {
	mu       sync.Mutex
	policies map[string]*sandbox.Policy
}

func newFakePolicySink() *fakePolicySink {
	return &fakePolicySink{policies: make(map[string]*sandbox.Policy)}
}

func (f *fakePolicySink) SetPolicy(name string, p *sandbox.Policy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policies[name] = p
}

func (f *fakePolicySink) get(name string) (*sandbox.Policy, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.policies[name]
	return p, ok
}

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

// TestRegistryScanLoadsSkillPolicyDir — when a skill ships a
// policy.d/ subtree AND a PolicySink is configured, the scanner
// applies its tool policies via sandbox.LoadSkillPolicies.
func TestRegistryScanLoadsSkillPolicyDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	skDir := filepath.Join(root, "greeter")
	_ = os.Mkdir(skDir, 0o755)
	_ = os.WriteFile(filepath.Join(skDir, "h.sh"), []byte("echo"), 0o755)
	_ = os.WriteFile(filepath.Join(skDir, "manifest.yaml"), []byte(`name: greeter
version: 1.0.0
runtime: bash
handler: h.sh`), 0o644)

	policyDir := filepath.Join(skDir, "policy.d")
	_ = os.Mkdir(policyDir, 0o755)
	_ = os.WriteFile(filepath.Join(policyDir, "greeter.toml"), []byte(`
name = "greeter"
allowed_paths = ["/tmp"]
`), 0o644)

	sink := newFakePolicySink()
	r := NewRegistry(nil)
	r.SetPolicySink(sink)

	errs := r.Scan(root)
	if len(errs) != 0 {
		t.Fatalf("scan errs: %v", errs)
	}
	if _, ok := sink.get("greeter"); !ok {
		t.Error("policy.d/greeter.toml should have been applied to the sink")
	}
}

// TestRegistryScanRejectsPoliciesOutsideOwnership — the ownership
// guard inside sandbox.LoadSkillPolicies rejects policies that name
// tools the skill doesn't own.
func TestRegistryScanRejectsPoliciesOutsideOwnership(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	skDir := filepath.Join(root, "greeter")
	_ = os.Mkdir(skDir, 0o755)
	_ = os.WriteFile(filepath.Join(skDir, "h.sh"), []byte("echo"), 0o755)
	_ = os.WriteFile(filepath.Join(skDir, "manifest.yaml"), []byte(`name: greeter
version: 1.0.0
runtime: bash
handler: h.sh`), 0o644)

	policyDir := filepath.Join(skDir, "policy.d")
	_ = os.Mkdir(policyDir, 0o755)
	// Skill "greeter" trying to ship a policy for "git" — must be
	// rejected by the ownership guard.
	_ = os.WriteFile(filepath.Join(policyDir, "git.toml"), []byte(`
name = "git"
allowed_paths = ["/tmp"]
`), 0o644)

	sink := newFakePolicySink()
	r := NewRegistry(nil)
	r.SetPolicySink(sink)

	_ = r.Scan(root)
	if _, ok := sink.get("git"); ok {
		t.Error("ownership guard should have blocked the foreign policy")
	}
}

// TestRegistryScanSkipsPoliciesWhenNoSink — without a PolicySink
// configured, a skill's policy.d/ is left untouched (no crash, no
// error). Preserves the sink-less test path used elsewhere.
func TestRegistryScanSkipsPoliciesWhenNoSink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	skDir := filepath.Join(root, "x")
	_ = os.Mkdir(skDir, 0o755)
	_ = os.WriteFile(filepath.Join(skDir, "h.sh"), []byte(""), 0o755)
	_ = os.WriteFile(filepath.Join(skDir, "manifest.yaml"), []byte(`name: x
version: 1.0.0
runtime: bash
handler: h.sh`), 0o644)
	_ = os.MkdirAll(filepath.Join(skDir, "policy.d"), 0o755)
	_ = os.WriteFile(filepath.Join(skDir, "policy.d", "x.toml"), []byte(`name = "x"`), 0o644)

	r := NewRegistry(nil) // no sink
	errs := r.Scan(root)
	if len(errs) != 0 {
		t.Errorf("scan without sink should be clean; got %v", errs)
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
