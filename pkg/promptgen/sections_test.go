package promptgen

import (
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestSectionFormatAtLevel(t *testing.T) {
	t.Parallel()
	s := Section{Title: "T", Body: "body\n"}
	if got := s.FormatAtLevel(2); !strings.HasPrefix(got, "## T\n\nbody\n") {
		t.Errorf("level=2 should use ##; got %q", got)
	}
	if got := s.FormatAtLevel(0); !strings.HasPrefix(got, "# T\n\n") {
		t.Errorf("level=0 should clamp to 1; got %q", got)
	}
}

func TestSectionFormatTrimsTrailingNewlines(t *testing.T) {
	t.Parallel()
	s := Section{Title: "X", Body: "hello\n\n\n"}
	got := s.Format()
	// Expected: "# X\n\nhello\n" — one trailing newline, not a stack.
	if got != "# X\n\nhello\n" {
		t.Errorf("unexpected format: %q", got)
	}
}

func TestBuildIdentityNilSoul(t *testing.T) {
	t.Parallel()
	s := BuildIdentity(nil)
	if s.Title != "Identity" {
		t.Errorf("Title: %q", s.Title)
	}
	if !strings.Contains(s.Body, "Default assistant persona") {
		t.Errorf("nil Soul should use default body; got %q", s.Body)
	}
}

// TestBuildIdentityOmitsName guards the deliberate convention:
// never include the soul's name in the rendered prompt. Names bias
// the LLM toward role-play; structured dimensions (formality,
// humour) shape behaviour without anchoring on a character.
func TestBuildIdentityOmitsName(t *testing.T) {
	t.Parallel()
	soul := &types.SoulConfig{
		Name:    "Jarvis",
		Scope:   "personal",
		Culture: "UK",
		EmotiveStyle: types.EmotiveStyle{
			Formality:  6,
			Directness: 8,
		},
		PersonaDescription: "A thoughtful assistant.",
	}
	s := BuildIdentity(soul)
	if strings.Contains(s.Body, "Jarvis") {
		t.Error("SECURITY: soul.Name must NOT appear in identity block")
	}
	if !strings.Contains(s.Body, "A thoughtful assistant.") {
		t.Error("persona description should appear")
	}
	if !strings.Contains(s.Body, "formality: 6/10") {
		t.Error("emotive scores should render as n/10")
	}
	if !strings.Contains(s.Body, "scope: personal") {
		t.Error("scope should appear")
	}
}

func TestBuildIdentitySkipsZeroScores(t *testing.T) {
	t.Parallel()
	soul := &types.SoulConfig{
		EmotiveStyle: types.EmotiveStyle{
			Formality:  5,
			Directness: 0, // zero — should not render
		},
	}
	s := BuildIdentity(soul)
	if !strings.Contains(s.Body, "formality") {
		t.Error("non-zero score should render")
	}
	if strings.Contains(s.Body, "directness") {
		t.Error("zero score should be elided to reduce prompt noise")
	}
}

func TestBuildSafetyMentionsUntrustedDelimiters(t *testing.T) {
	t.Parallel()
	s := BuildSafety()
	if !strings.Contains(s.Body, "<untrusted>") {
		t.Error("safety block should teach the model about <untrusted> delimiters")
	}
	if !strings.Contains(strings.ToLower(s.Body), "hard to reverse") {
		t.Error("safety should cover confirmation-before-destructive-action")
	}
}

func TestBuildSafetyIsStable(t *testing.T) {
	t.Parallel()
	// Two back-to-back calls must produce identical output — the
	// section is static so the cache layer can rely on it. Each call
	// is captured to a local first (staticcheck flags a.B != a.B as
	// a pointless comparison if inlined).
	first := BuildSafety().Body
	second := BuildSafety().Body
	if first != second {
		t.Error("safety block must be deterministic")
	}
}

func TestBuildToolingEmpty(t *testing.T) {
	t.Parallel()
	s := BuildTooling(nil)
	if !strings.Contains(s.Body, "(none configured)") {
		t.Errorf("empty list should say so; got %q", s.Body)
	}
}

func TestBuildToolingSortedByName(t *testing.T) {
	t.Parallel()
	tools := []ToolInfo{
		{Name: "zebra", Description: "z", RiskTier: "low"},
		{Name: "alpha", Description: "a", RiskTier: "low"},
		{Name: "mid", Description: "m"},
	}
	s := BuildTooling(tools)
	// Alphabetical order surface check via substring positions.
	posAlpha := strings.Index(s.Body, "alpha")
	posMid := strings.Index(s.Body, "mid")
	posZebra := strings.Index(s.Body, "zebra")
	if !(posAlpha < posMid && posMid < posZebra) {
		t.Errorf("tools not sorted by name: body=%q", s.Body)
	}
	if !strings.Contains(s.Body, "`low`") {
		t.Error("risk tier should render in backticks when set")
	}
}

func TestBuildSkillsEmpty(t *testing.T) {
	t.Parallel()
	s := BuildSkills(nil)
	if !strings.Contains(s.Body, "(none installed)") {
		t.Error("empty skill list should surface cleanly")
	}
}

func TestBuildSkillsSortedByName(t *testing.T) {
	t.Parallel()
	skills := []SkillInfo{
		{Name: "writer", Description: "w", Location: "/opt/skills/writer"},
		{Name: "reader", Description: "r"},
	}
	s := BuildSkills(skills)
	posReader := strings.Index(s.Body, "reader")
	posWriter := strings.Index(s.Body, "writer")
	if !(posReader < posWriter) {
		t.Errorf("sorted-by-name violated: %q", s.Body)
	}
}

func TestBuildCurrentTimeTimezoneOnly(t *testing.T) {
	t.Parallel()
	// Fixed time so the assertions are stable.
	now := time.Date(2026, 4, 23, 10, 30, 0, 0, time.UTC)
	london, _ := time.LoadLocation("Europe/London")
	s := BuildCurrentTime(now, london)
	if !strings.Contains(s.Body, "Europe/London") {
		t.Errorf("timezone should render: %q", s.Body)
	}
	// April 23, 2026 is a Thursday in London.
	if !strings.Contains(s.Body, "Thursday") {
		t.Errorf("weekday should render: %q", s.Body)
	}
	if !strings.Contains(s.Body, "2026-04-23") {
		t.Errorf("date should render: %q", s.Body)
	}
	// Hour/minute MUST NOT render — per convention, exact wall-clock
	// bloats the cache layer (every turn looks unique).
	if strings.Contains(s.Body, "10:30") {
		t.Error("exact time should NOT appear in the prompt")
	}
}

func TestBuildCurrentTimeNilLocationFallsBackToUTC(t *testing.T) {
	t.Parallel()
	s := BuildCurrentTime(time.Now(), nil)
	if !strings.Contains(s.Body, "UTC") {
		t.Error("nil tz should fall back to UTC")
	}
}

func TestBuildRuntimeRendersAllFields(t *testing.T) {
	t.Parallel()
	s := BuildRuntime(RuntimeInfo{
		Hostname: "node-a",
		OS:       "linux",
		NodeID:   "a1b2c3",
		Model:    "claude-sonnet-4-6",
	})
	for _, want := range []string{"node-a", "linux", "a1b2c3", "claude-sonnet-4-6"} {
		if !strings.Contains(s.Body, want) {
			t.Errorf("runtime missing %q: %q", want, s.Body)
		}
	}
}

func TestBuildRuntimeEmptyGracefully(t *testing.T) {
	t.Parallel()
	s := BuildRuntime(RuntimeInfo{})
	if !strings.Contains(s.Body, "unavailable") {
		t.Error("empty runtime should surface cleanly (not an empty block)")
	}
}

func TestBuildWorkspaceDefaultPath(t *testing.T) {
	t.Parallel()
	s := BuildWorkspace("")
	if !strings.Contains(s.Body, "/var/lobslaw/workspace") {
		t.Errorf("default path expected; got %q", s.Body)
	}
}

func TestBuildWorkspaceCustomPath(t *testing.T) {
	t.Parallel()
	s := BuildWorkspace("/app/data/workspace")
	if !strings.Contains(s.Body, "/app/data/workspace") {
		t.Errorf("custom path should render: %q", s.Body)
	}
}
