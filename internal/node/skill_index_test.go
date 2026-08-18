package node

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/skills"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/promptgen"
)

// GenerateInput.Skills existed and BuildSkills rendered it, and
// nothing ever populated it — so "Installed Skills" said "(none
// installed)" on every turn no matter what was installed, and a skill
// could only be invoked by a model that guessed its name.

func indexNode(t *testing.T, providers []config.ProviderConfig, manifests ...skills.Manifest) *Node {
	t.Helper()
	reg := skills.NewRegistry(slog.New(slog.DiscardHandler))
	for i := range manifests {
		m := manifests[i]
		reg.Put(&skills.Skill{Manifest: m, ManifestDir: "/skills/" + m.Name})
	}
	return &Node{
		skillRegistry: reg,
		log:           slog.New(slog.DiscardHandler),
		cfg: Config{
			Compute: config.ComputeConfig{Providers: providers},
		},
	}
}

func indexNames(index []promptgen.SkillInfo) []string {
	out := make([]string, 0, len(index))
	for _, s := range index {
		out = append(out, s.Name)
	}
	return out
}

// The acceptance criterion semantic top-K cannot offer: every runnable
// skill is listed, every time, whatever the user said.
func TestIndexListsEveryRunnableSkill(t *testing.T) {
	t.Parallel()
	n := indexNode(t, nil,
		skills.Manifest{Name: "alpha", Description: "does alpha"},
		skills.Manifest{Name: "beta", Description: "does beta"},
		skills.Manifest{Name: "gamma", Description: "does gamma"},
	)

	index := n.skillIndexProvider()()
	if len(index) != 3 {
		t.Fatalf("index = %v, want all three", indexNames(index))
	}
	// Repeated calls are identical: the index is not a function of the
	// turn, which is the whole point.
	if second := n.skillIndexProvider()(); len(second) != 3 {
		t.Errorf("a second call returned %v", indexNames(second))
	}
}

// A skill that could not run here is not a capability the model is
// missing out on — listing it teaches the model something false.
func TestIndexDropsSkillsThatCannotRunHere(t *testing.T) {
	t.Parallel()
	n := indexNode(t,
		[]config.ProviderConfig{{Label: "openai", Capabilities: []string{"chat"}}},
		skills.Manifest{Name: "text-thing", Description: "works anywhere"},
		skills.Manifest{Name: "vision-thing", Description: "reads screenshots",
			RequiresCapability: []string{"vision"}},
	)

	index := n.skillIndexProvider()()
	names := indexNames(index)
	if len(index) != 1 || names[0] != "text-thing" {
		t.Errorf("index = %v; a vision skill was advertised on a text-only deployment", names)
	}
}

// ...and appears once the deployment gains the capability, without
// anything being reinstalled.
func TestIndexIncludesSkillsOnceTheCapabilityExists(t *testing.T) {
	t.Parallel()
	n := indexNode(t,
		[]config.ProviderConfig{
			{Label: "openai", Capabilities: []string{"chat"}},
			{Label: "vision-provider", Capabilities: []string{"vision"}},
		},
		skills.Manifest{Name: "vision-thing", Description: "reads screenshots",
			RequiresCapability: []string{"vision"}},
	)
	if index := n.skillIndexProvider()(); len(index) != 1 {
		t.Errorf("index = %v; the capability is configured", indexNames(index))
	}
}

// References are named, never included. An index that grew with body
// size would defeat the point of disclosing progressively.
func TestIndexNamesReferencesWithoutReadingThem(t *testing.T) {
	t.Parallel()
	n := indexNode(t, nil, skills.Manifest{
		Name: "researcher", Description: "does research",
		References: []skills.Reference{{Path: "references/api.md"}, {Path: "templates/report.md"}},
	})

	index := n.skillIndexProvider()()
	if len(index) != 1 {
		t.Fatalf("index = %v", indexNames(index))
	}
	if len(index[0].References) != 2 {
		t.Errorf("references = %v", index[0].References)
	}

	rendered := promptgen.BuildSkills(index, 0).Body
	if !strings.Contains(rendered, "references/api.md") {
		t.Errorf("the index does not say what the skill carries:\n%s", rendered)
	}
	// Named only — a path, not a document.
	if strings.Contains(rendered, "# API") {
		t.Errorf("the index inlined a reference body:\n%s", rendered)
	}
}

// The rendered index has to tell the model the list is complete.
// Without that it fills gaps by guessing, which is the failure mode
// this whole design exists to avoid.
func TestRenderedIndexClaimsCompleteness(t *testing.T) {
	t.Parallel()
	body := promptgen.BuildSkills([]promptgen.SkillInfo{
		{Name: "alpha", Description: "does alpha"},
	}, 0).Body
	if !strings.Contains(strings.ToLower(body), "complete") {
		t.Errorf("the index does not tell the model it is exhaustive:\n%s", body)
	}
}

func TestNilRegistryYieldsNoProvider(t *testing.T) {
	t.Parallel()
	n := &Node{log: slog.New(slog.DiscardHandler)}
	if n.skillIndexProvider() != nil {
		t.Error("a node with no skill registry returned a provider")
	}
}
