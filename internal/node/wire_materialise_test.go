package node

import (
	"path/filepath"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/skills"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// The translation from store record to materialiser input. Small, and
// exactly the place a schema change would break quietly: a field
// dropped here means a skill that materialises with no description, or
// with no bundled files, and nothing fails.

func TestArtefactsCarryEverythingTheSkillNeeds(t *testing.T) {
	t.Parallel()
	got := artefactsFor([]*lobslawv1.SelfTaughtRecord{{
		Id:          "skill:tidy",
		Name:        "tidy",
		Description: "how this user likes things tidied",
		Body:        "the procedure",
		Version:     7,
		Files:       map[string]string{"references/api.md": "content"},
	}})
	if len(got) != 1 {
		t.Fatalf("got %d artefacts", len(got))
	}
	want := skills.Artefact{
		Name:        "tidy",
		Description: "how this user likes things tidied",
		Body:        "the procedure",
		Version:     7,
		Files:       map[string]string{"references/api.md": "content"},
	}
	if got[0].Name != want.Name || got[0].Description != want.Description ||
		got[0].Body != want.Body || got[0].Version != want.Version {
		t.Errorf("artefact = %+v, want %+v", got[0], want)
	}
	if got[0].Files["references/api.md"] != "content" {
		t.Errorf("files = %v", got[0].Files)
	}
}

// A refinement awaiting approval is a proposal. Materialising it would
// put it in the prompt, which is precisely what proposing instead of
// applying exists to prevent.
func TestAPendingRefinementIsNotMaterialised(t *testing.T) {
	t.Parallel()
	got := artefactsFor([]*lobslawv1.SelfTaughtRecord{{
		Name:    "tidy",
		Body:    "the approved procedure",
		Version: 1,
		Pending: &lobslawv1.PendingRevision{
			Body:        "a suggestion nobody has accepted",
			Description: "the suggested description",
		},
	}})
	if got[0].Body != "the approved procedure" {
		t.Errorf("body = %q; a pending refinement reached the cache", got[0].Body)
	}
}

// data_dir is relative in every example config, and raft resolves it
// against the working directory without complaint. The materialiser
// insists on an absolute root — rightly, it prunes directories — so a
// node with the documented relative data_dir logged
//
//	skills: materialiser failed to start
//	skills materialiser: materialiser root "data/skills-cache" must be absolute
//
// at boot and carried on with self-learning ENABLED and nothing able
// to materialise. An error nobody reads, and a feature that is on and
// cannot work.
func TestARelativeDataDirStillGivesTheMaterialiserAnAbsoluteRoot(t *testing.T) {
	for _, dataDir := range []string{"data", "./data", "var/lib/lobslaw"} {
		root, err := skillsCacheRoot(dataDir)
		if err != nil {
			t.Fatalf("%q: %v", dataDir, err)
		}
		if _, err := skills.NewMaterialiser(root, nil); err != nil {
			t.Errorf("relative data_dir %q was refused: %v", dataDir, err)
		}
	}
}

// An absolute data_dir must be left exactly as given — resolving one
// that is already resolved would be a no-op at best and, if it ever
// stopped being one, would relocate a node's skill cache on upgrade.
func TestAnAbsoluteDataDirIsUnchanged(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "data")
	root, err := skillsCacheRoot(abs)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(abs, skillsCacheDirName); root != want {
		t.Errorf("root = %q, want %q", root, want)
	}
}

// The agent scanner must point at the agent SUBTREE.
//
// It was given the cache root, which contains agent/ and imported/.
// The scan found two directories with no manifest between them,
// registered nothing, and every skill the agent taught itself
// materialised to disk and stayed invisible: `skills list` said "no
// skills installed" and skill_view never registered, on a node whose
// cache held the skill.
//
// AgentRoot exists so a caller never reconstructs the path — passing
// its PARENT was the one way left to get it wrong, and it looks
// entirely plausible on the line.
func TestTheAgentScanPointsAtTheAgentSubtree(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "skills-cache")
	m, err := skills.NewMaterialiser(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.AgentRoot() == m.Root() {
		t.Fatal("AgentRoot is the cache root; the subtrees are not separated")
	}
	if filepath.Base(m.AgentRoot()) != skills.AgentSubtree {
		t.Errorf("AgentRoot = %q, want it to end in %q", m.AgentRoot(), skills.AgentSubtree)
	}
	// The imported subtree is a sibling, so a scan pointed at the
	// parent would sweep both and honour neither's provenance.
	if filepath.Dir(m.ImportedRoot()) != m.Root() {
		t.Errorf("ImportedRoot %q is not a sibling under %q", m.ImportedRoot(), m.Root())
	}
}
