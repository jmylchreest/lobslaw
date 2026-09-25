package skills

import (
	"crypto/ed25519"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/memory"
)

// The store is the authority for imported skills; the cache is where
// they can be executed. The property that decides whether the move is
// possible at all is the same one the store itself rests on: a SIGNED
// skill has to arrive on disk byte-identical, or its signature stops
// verifying and a SigningRequire deployment rejects it.

const storedManifest = `name: tidy
version: 1.2.3
runtime: python
handler: handler.py
`

func stored(name, version string) StoredSkill {
	return StoredSkill{
		Name: name, Version: version,
		ManifestYAML: []byte(strings.ReplaceAll(storedManifest, "1.2.3", version)),
		Files:        map[string][]byte{"handler.py": []byte("print('hi')")},
	}
}

func TestAStoredSkillMaterialisesAndParses(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	res, err := m.MaterialiseStored([]StoredSkill{stored("tidy", "1.2.3")})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written) != 1 {
		t.Fatalf("written = %v", res.Written)
	}

	dir := filepath.Join(m.ImportedRoot(), "tidy", "1.2.3")
	skill, err := Parse(dir)
	if err != nil {
		t.Fatalf("the materialised skill does not parse: %v", err)
	}
	if skill.Manifest.Name != "tidy" || skill.Manifest.Version != "1.2.3" {
		t.Errorf("manifest = %+v", skill.Manifest)
	}
}

// The whole point. The manifest reaches disk unchanged, so a signature
// over the original bytes still verifies against the file the
// interpreter will load.
func TestASignedSkillStillVerifiesOnDisk(t *testing.T) {
	t.Parallel()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately not already-normalised: a comment, an unusual key
	// order and trailing whitespace, all of which any re-render would
	// tidy away.
	//
	// handler_sha256 is pinned because SigningRequire refuses a signed
	// manifest without it — R19's rule that a signature naming a script
	// but not its digest covers no executable content. A fixture that
	// omitted it would be exercising a manifest no signed deployment
	// would accept.
	manifest := "# publisher: example.com\nversion: 1.2.3\nname: tidy\n" +
		"runtime: python\nhandler: handler.py\n" +
		"handler_sha256: c2d0a5e0790d97a015387a995c0d0b5eb3e88138466586fc980787c9b1731eb8   \n"
	sk := stored("tidy", "1.2.3")
	sk.ManifestYAML = []byte(manifest)
	sk.ManifestSig = ed25519.Sign(priv, []byte(manifest))

	m := materialiser(t)
	if _, err := m.MaterialiseStored([]StoredSkill{sk}); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(m.ImportedRoot(), "tidy", "1.2.3")
	onDisk, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(filepath.Join(dir, "manifest.yaml.sig"))
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, onDisk, sig) {
		t.Fatal("the signature does not verify against the materialised manifest — " +
			"the bytes were not written verbatim")
	}

	// And the registry agrees, through the real signing path.
	v := NewVerifier()
	if err := v.AddKey("example", pub); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(slog.New(slog.DiscardHandler))
	if errs := r.ScanImported(m.ImportedRoot(), SigningRequire, v); len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	got, err := r.Get("tidy")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsSigned {
		t.Error("the skill did not register as signed")
	}
	// The EFFECTIVE tier. Parse deliberately leaves the field underived
	// and tierOf derives it from the verification result, so asserting
	// the field would pin the wrong thing.
	if tierOf(got) != TierSigned {
		t.Errorf("tier = %v, want signed", tierOf(got))
	}
}

// An empty .sig file makes an unsigned skill look signed and then fail
// verification, which is worse than having no file at all.
func TestAnUnsignedSkillGetsNoSignatureFile(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	if _, err := m.MaterialiseStored([]StoredSkill{stored("tidy", "1.2.3")}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.ImportedRoot(), "tidy", "1.2.3", "manifest.yaml.sig")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("an unsigned skill wrote a signature file")
	}
}

// --- the two subtrees are separate ------------------------------------

// The tier must not depend on which scanner reached a directory first.
func TestTheTwoSubtreesDoNotCollide(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	if _, err := m.MaterialiseStored([]StoredSkill{stored("tidy", "1.2.3")}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Materialise([]Artefact{artefact("tidy", "the agent version", 1)}); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(slog.New(slog.DiscardHandler))
	if errs := r.ScanAgent(m.AgentRoot()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if errs := r.ScanImported(m.ImportedRoot(), SigningOff, nil); len(errs) != 0 {
		t.Fatal(errs)
	}

	// Both are candidates for the same name; the imported one wins on
	// tier, which is the precedence rule doing its job.
	got, err := r.Get("tidy")
	if err != nil {
		t.Fatal(err)
	}
	if tierOf(got) != TierOperator {
		t.Errorf("tier = %v; the agent version won a contested name", tierOf(got))
	}
	if !strings.Contains(got.ManifestDir, ImportedSubtree) {
		t.Errorf("winner dir = %q", got.ManifestDir)
	}
}

// Pruning one subtree must not touch the other.
func TestPruningTheStoreLeavesAgentSkillsAlone(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	if _, err := m.Materialise([]Artefact{artefact("mine", "body", 1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MaterialiseStored([]StoredSkill{stored("tidy", "1.2.3")}); err != nil {
		t.Fatal(err)
	}
	// The store now holds nothing.
	if _, err := m.MaterialiseStored(nil); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(m.AgentRoot(), "mine", "0.0.1")); err != nil {
		t.Errorf("an agent skill was pruned by a store reconcile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.ImportedRoot(), "tidy")); !os.IsNotExist(err) {
		t.Error("the imported skill was not pruned")
	}
}

// --- convergence ------------------------------------------------------

func TestASupersededStoreVersionIsPruned(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	if _, err := m.MaterialiseStored([]StoredSkill{stored("tidy", "1.0.0")}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MaterialiseStored([]StoredSkill{stored("tidy", "2.0.0")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.ImportedRoot(), "tidy", "1.0.0")); !os.IsNotExist(err) {
		t.Error("the old version survived")
	}
}

func TestAnEditedStoreCacheIsCorrected(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	sk := stored("tidy", "1.2.3")
	if _, err := m.MaterialiseStored([]StoredSkill{sk}); err != nil {
		t.Fatal(err)
	}
	handler := filepath.Join(m.ImportedRoot(), "tidy", "1.2.3", "handler.py")
	if err := os.WriteFile(handler, []byte("os.system('rm -rf /')"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MaterialiseStored([]StoredSkill{sk}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(handler)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "print('hi')" {
		t.Errorf("handler = %q; an edit on disk survived a reconcile", got)
	}
}

func TestAnUnchangedStorePassRewritesNothing(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	sk := []StoredSkill{stored("tidy", "1.2.3")}
	if _, err := m.MaterialiseStored(sk); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.ImportedRoot(), "tidy", "1.2.3", "manifest.yaml")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.MaterialiseStored(sk); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("an unchanged store pass rewrote the directory")
	}
}

// --- refusals ---------------------------------------------------------

// The VERSION becomes a path segment here, unlike the agent side where
// it is generated from a counter. It comes from a manifest somebody
// else wrote, so it is checked.
func TestAVersionThatWouldEscapeIsRefused(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"../escape", "a/b", "..", ".hidden", ""} {
		m := materialiser(t)
		sk := stored("tidy", "1.2.3")
		sk.Version = version
		res, err := m.MaterialiseStored([]StoredSkill{sk})
		if err != nil {
			t.Fatalf("%q: %v", version, err)
		}
		if _, refused := res.Refused["tidy"]; !refused {
			t.Errorf("version %q was accepted", version)
		}
	}
}

func TestANameThatWouldEscapeIsRefused(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"../escape", `a\b`, ".", ""} {
		m := materialiser(t)
		sk := stored(name, "1.2.3")
		res, err := m.MaterialiseStored([]StoredSkill{sk})
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if _, refused := res.Refused[name]; !refused {
			t.Errorf("name %q was accepted", name)
		}
	}
}

// The documented convention for a bundled skill's body is SKILL.md
// (skill.go's Body field comment). The materialiser writes only the
// manifest and its signature for a stored skill, so a bundled SKILL.md
// is the skill's own body, not a name the materialiser owns.
func TestAStoredSkillWithASKILLBodyMaterialises(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	sk := stored("tidy", "1.2.3")
	sk.Files[BodyFile] = []byte("# tidy\n\nhow to tidy up")
	res, err := m.MaterialiseStored([]StoredSkill{sk})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written) != 1 {
		t.Fatalf("written = %v, refused = %v", res.Written, res.Refused)
	}
	got, err := os.ReadFile(filepath.Join(m.ImportedRoot(), "tidy", "1.2.3", BodyFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "# tidy\n\nhow to tidy up" {
		t.Errorf("body = %q", got)
	}
}

// The repo's own remote-dispatch skill uses body: SKILL.md, the
// convention the fix restores for imported skills. Materialising it
// through the store path, rather than a synthetic fixture, is the
// strongest evidence the refusal is gone: a manifest whose body_sha256
// is pinned against real content only parses if the bytes on disk are
// exactly what was imported.
func TestAStoredSkillFromTheRepoBundleMaterialises(t *testing.T) {
	t.Parallel()
	src := filepath.Join("..", "..", "bundles", "lobslaw-core", "remote-dispatch")
	if _, err := os.Stat(src); os.IsNotExist(err) {
		t.Skip("no bundles/lobslaw-core/remote-dispatch in this tree")
	}
	bundle, err := memory.ReadBundle(src)
	if err != nil {
		t.Fatal(err)
	}

	m := materialiser(t)
	res, err := m.MaterialiseStored([]StoredSkill{{
		Name:         "remote-dispatch",
		Version:      "1.0.0",
		ManifestYAML: bundle.Manifest,
		ManifestSig:  bundle.Signature,
		Files:        bundle.Files,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written) != 1 {
		t.Fatalf("written = %v, refused = %v", res.Written, res.Refused)
	}

	dir := filepath.Join(m.ImportedRoot(), "remote-dispatch", "1.0.0")
	skill, err := Parse(dir)
	if err != nil {
		t.Fatalf("the materialised skill does not parse: %v", err)
	}
	if skill.Manifest.Name != "remote-dispatch" {
		t.Errorf("manifest = %+v", skill.Manifest)
	}
}

// A path can be shaped for traversal even though it never names
// something the materialiser owns; that check runs regardless of
// ownership.
func TestAStoredBundledFileCannotEscapeTheSkillDirectory(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/etc/passwd", ""} {
		m := materialiser(t)
		sk := stored("tidy", "1.2.3")
		sk.Files[path] = []byte("payload")
		res, err := m.MaterialiseStored([]StoredSkill{sk})
		if err != nil {
			t.Fatalf("%q: %v", path, err)
		}
		if _, refused := res.Refused["tidy"]; !refused {
			t.Errorf("bundled path %q was accepted", path)
		}
	}
}

// A record naming a file that would overwrite the manifest or its
// signature would let a bundled file replace the thing the signature
// is over.
func TestABundledFileCannotOverwriteTheManifestOrSignature(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"manifest.yaml", "manifest.yaml.sig", "../out.md"} {
		m := materialiser(t)
		sk := stored("tidy", "1.2.3")
		sk.Files[path] = []byte("payload")
		res, err := m.MaterialiseStored([]StoredSkill{sk})
		if err != nil {
			t.Fatalf("%q: %v", path, err)
		}
		if _, refused := res.Refused["tidy"]; !refused {
			t.Errorf("bundled path %q was accepted", path)
		}
	}
}

func TestARecordWithNoManifestIsRefused(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	sk := stored("tidy", "1.2.3")
	sk.ManifestYAML = nil
	res, err := m.MaterialiseStored([]StoredSkill{sk})
	if err != nil {
		t.Fatal(err)
	}
	if _, refused := res.Refused["tidy"]; !refused {
		t.Error("a record with no manifest was materialised")
	}
}

// One bad record must not take the rest of the library down.
func TestOneRefusedStoreRecordDoesNotStopTheRest(t *testing.T) {
	t.Parallel()
	m := materialiser(t)
	bad := stored("../escape", "1.0.0")
	res, err := m.MaterialiseStored([]StoredSkill{bad, stored("tidy", "1.2.3")})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written) != 1 {
		t.Fatalf("written = %v", res.Written)
	}
	if len(res.Refused) != 1 {
		t.Errorf("refused = %v", res.Refused)
	}
}
