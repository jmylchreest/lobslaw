package promptgen

import (
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestGenerateZeroInputStillProducesSections(t *testing.T) {
	t.Parallel()
	got := Generate(GenerateInput{})
	// Safety and identity should always render — the minimum viable
	// system prompt.
	if !strings.Contains(got, "# Identity") {
		t.Error("Identity section missing")
	}
	if !strings.Contains(got, "# Operating Principles") {
		t.Error("Operating Principles (safety) missing")
	}
	if !strings.Contains(got, "# Runtime") {
		t.Error("Runtime section missing")
	}
	if !strings.Contains(got, "# Workspace") {
		t.Error("Workspace section missing")
	}
}

func TestGenerateWithSoul(t *testing.T) {
	t.Parallel()
	soul := &types.SoulConfig{
		Name:               "Jarvis", // MUST NOT appear
		Scope:              "personal",
		PersonaDescription: "A helpful assistant.",
	}
	got := Generate(GenerateInput{Soul: soul})
	if strings.Contains(got, "Jarvis") {
		t.Error("SECURITY: soul name must not appear in the assembled prompt")
	}
	if !strings.Contains(got, "A helpful assistant.") {
		t.Error("persona description missing")
	}
	if !strings.Contains(got, "scope: personal") {
		t.Error("scope field missing")
	}
}

func TestGenerateSectionOrder(t *testing.T) {
	t.Parallel()
	got := Generate(GenerateInput{
		Tools:  []ToolInfo{{Name: "t", Description: "tool"}},
		Skills: []SkillInfo{{Name: "s", Description: "skill"}},
	})
	positions := map[string]int{
		"Identity":             strings.Index(got, "# Identity"),
		"Operating Principles": strings.Index(got, "# Operating Principles"),
		"Current Time":         strings.Index(got, "# Current Time"),
		"Runtime":              strings.Index(got, "# Runtime"),
		"Workspace":            strings.Index(got, "# Workspace"),
		"Available Tools":      strings.Index(got, "# Available Tools"),
		"Installed Skills":     strings.Index(got, "# Installed Skills"),
	}
	order := []string{
		"Identity", "Operating Principles", "Available Tools",
		"Installed Skills", "Current Time", "Runtime", "Workspace",
	}
	for i := 1; i < len(order); i++ {
		prev, curr := positions[order[i-1]], positions[order[i]]
		if prev < 0 || curr < 0 {
			t.Errorf("section %q or %q missing", order[i-1], order[i])
			continue
		}
		if prev >= curr {
			t.Errorf("section order violated: %q (%d) should come before %q (%d)",
				order[i-1], prev, order[i], curr)
		}
	}
}

// TestGenerateDeterministicAcrossRuns — two calls with identical
// input must produce byte-identical output. Provider-side prompt
// caches key on exact bytes; drift kills the cache hit rate.
func TestGenerateDeterministicAcrossRuns(t *testing.T) {
	t.Parallel()
	in := GenerateInput{
		Soul: &types.SoulConfig{PersonaDescription: "stable"},
		Tools: []ToolInfo{
			{Name: "b", Description: "later-alphabetical"},
			{Name: "a", Description: "earlier-alphabetical"},
		},
		Now:      time.Date(2026, 4, 23, 0, 0, 0, 0, time.UTC),
		Timezone: time.UTC,
	}
	first := Generate(in)
	second := Generate(in)
	if first != second {
		t.Error("same input → different output; prompt cache would drift")
	}
}

func TestGenerateBootstrapAppended(t *testing.T) {
	t.Parallel()
	got := Generate(GenerateInput{
		Bootstrap: BootstrapConfig{
			Paths: []string{"snippet"},
			FS:    MapFS{"snippet": "user-supplied context"},
		},
	})
	if !strings.Contains(got, "# Bootstrap") {
		t.Error("Bootstrap section header missing")
	}
	if !strings.Contains(got, "user-supplied context") {
		t.Error("bootstrap content missing")
	}
	if !strings.Contains(got, "<!-- bootstrap: snippet -->") {
		t.Error("bootstrap file heading missing")
	}
}

func TestGenerateBootstrapElidedWhenEmpty(t *testing.T) {
	t.Parallel()
	got := Generate(GenerateInput{Bootstrap: BootstrapConfig{Paths: nil}})
	if strings.Contains(got, "# Bootstrap") {
		t.Error("Bootstrap section should not render when Paths is empty")
	}
}

// TestGenerateContextWrappedWithTrustDelimiters — context blocks
// MUST go through WrapContext so the model's safety training can
// refuse embedded instructions in untrusted regions.
func TestGenerateContextWrappedWithTrustDelimiters(t *testing.T) {
	t.Parallel()
	got := Generate(GenerateInput{
		Context: []ContextBlock{
			{Source: "tool:bash", Trust: TrustUntrusted, Content: "some stdout"},
		},
	})
	// Default (empty) category → short-term → "Recent Context" header.
	if !strings.Contains(got, "# Recent Context") {
		t.Error("Recent Context section missing")
	}
	if !strings.Contains(got, `<untrusted source="tool:bash">`) {
		t.Errorf("untrusted wrapper missing; got:\n%s", got)
	}
	if !strings.Contains(got, "some stdout") {
		t.Error("context body missing")
	}
}

func TestGenerateContextSplitsShortAndLongTerm(t *testing.T) {
	t.Parallel()
	got := Generate(GenerateInput{
		Context: []ContextBlock{
			{Source: "session:turn", Category: CategoryShortTerm, Trust: TrustUntrusted, Content: "recent discussion"},
			{Source: "memory:recall", Category: CategoryLongTerm, Trust: TrustUntrusted, Content: "past session"},
		},
	})
	if !strings.Contains(got, "# Recent Context") {
		t.Error("Recent Context section missing for short-term block")
	}
	if !strings.Contains(got, "# Recalled Memory") {
		t.Error("Recalled Memory section missing for long-term block")
	}
	if strings.Index(got, "# Recent Context") >= strings.Index(got, "# Recalled Memory") {
		t.Error("Recent Context should appear before Recalled Memory")
	}
}

func TestGenerateContextElidedWhenEmpty(t *testing.T) {
	t.Parallel()
	got := Generate(GenerateInput{Context: nil})
	if strings.Contains(got, "# Recent Context") || strings.Contains(got, "# Recalled Memory") {
		t.Error("Context sections should not render when blocks is empty")
	}
}

func TestGenerateZeroNowFallsBackToTimeNow(t *testing.T) {
	t.Parallel()
	got := Generate(GenerateInput{}) // no Now set
	// Should include a date — the exact value depends on when the
	// test runs, so just assert the section exists non-empty.
	if !strings.Contains(got, "# Current Time") {
		t.Error("Current Time missing")
	}
	if !strings.Contains(got, "date:") {
		t.Error("time section should populate 'date:'")
	}
}

// TestGenerateToolsSkillsAndRuntimeIntegration — smoke-test the
// end-to-end assembly with representative real-ish inputs.
func TestGenerateToolsSkillsAndRuntimeIntegration(t *testing.T) {
	t.Parallel()
	got := Generate(GenerateInput{
		Tools: []ToolInfo{
			{Name: "bash", Description: "shell commands", RiskTier: "high"},
			{Name: "grep", Description: "search"},
		},
		Skills: []SkillInfo{
			{Name: "code-review", Description: "review diffs", Location: "/skills/code-review"},
		},
		Runtime: RuntimeInfo{Hostname: "host-a", OS: "linux", NodeID: "node-1", Model: "claude-sonnet-4-6"},
	})
	for _, want := range []string{
		"**bash** (`high`)", "**grep**",
		"**code-review**", "/skills/code-review",
		"host-a", "linux", "node-1", "claude-sonnet-4-6",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected prompt to contain %q", want)
		}
	}
}

func TestGenerateWorkspaceCustomPath(t *testing.T) {
	t.Parallel()
	got := Generate(GenerateInput{Workspace: "/app/data/ws"})
	if !strings.Contains(got, "/app/data/ws") {
		t.Errorf("custom workspace missing: %s", got)
	}
}
