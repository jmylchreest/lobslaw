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
	identity := BuildIdentity(soul)
	personality := BuildPersonality(soul, nil)
	if strings.Contains(identity.Body, "Jarvis") || strings.Contains(personality.Body, "Jarvis") {
		t.Error("SECURITY: soul.Name must NOT appear in identity or personality blocks")
	}
	if !strings.Contains(identity.Body, "A thoughtful assistant.") {
		t.Error("persona description should appear in Identity")
	}
	// Prose, not the dial that produced it. The score is the
	// operator's interface and never the model's — see
	// BuildPersonality.
	if !strings.Contains(personality.Body, "Write plainly") {
		t.Error("a mid formality should render as guidance in Personality")
	}
	if strings.Contains(personality.Body, "6/10") {
		t.Error("a raw dial value reached the prompt; models read those back")
	}
	if !strings.Contains(identity.Body, "scope: personal") {
		t.Error("scope should appear in Identity")
	}
}

func TestBuildPersonalitySkipsZeroScores(t *testing.T) {
	t.Parallel()
	soul := &types.SoulConfig{
		EmotiveStyle: types.EmotiveStyle{
			Formality:  5,
			Directness: 0, // unset — should not render
		},
	}
	s := BuildPersonality(soul, nil)
	if !strings.Contains(s.Body, "Write plainly") {
		t.Error("a set dial should render its guidance")
	}
	// An unset dial contributes nothing. Asserted on the guidance
	// rather than the word "directness", which no longer appears in
	// the prompt at any score.
	for _, band := range []string{"Ease into things", "Say what you mean", "Lead with the answer"} {
		if strings.Contains(s.Body, band) {
			t.Errorf("an unset dial rendered guidance: %q", band)
		}
	}
}

// The Telegram regression: a reply that agreed to its own instructions
// and then described them. Both halves are prompt tokens, so the fix is
// that neither is in the prompt to be read back.
func TestPersonalityCarriesNoDialVocabulary(t *testing.T) {
	t.Parallel()
	soul := &types.SoulConfig{
		EmotiveStyle: types.EmotiveStyle{
			EmojiUsage: "minimal",
			Excitement: 5,
			Formality:  2,
			Directness: 3,
			Sarcasm:    4,
			Humor:      7,
		},
	}
	body := BuildPersonality(soul, nil).Body
	for _, leak := range []string{
		"3/10", "7/10", "2/10", "4/10", "5/10",
		"emoji_usage", "directness", "formality", "excitement", "sarcasm",
	} {
		if strings.Contains(body, leak) {
			t.Errorf("configuration vocabulary %q reached the prompt", leak)
		}
	}
	// ...and the guidance it was replaced by is actually there.
	for _, want := range []string{
		"Do not use emoji.",
		"Write casually",
		"Ease into things",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing guidance %q", want)
		}
	}
}

// An unrecognised emoji_usage renders nothing rather than guessing.
func TestEmojiRuleIgnoresUnknownValues(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"", "reduced", "lots", "  "} {
		if got := emojiRule(v); got != "" {
			t.Errorf("emojiRule(%q) = %q, want empty", v, got)
		}
	}
	if emojiRule("  MINIMAL ") != "Do not use emoji." {
		t.Error("emoji_usage should be matched case- and space-insensitively")
	}
}

// The contract has to be in the assembled prompt, above the imperatives
// it governs, or it is describing a document the model has already read.
func TestPromptContractPrecedesTheInstructionsItGoverns(t *testing.T) {
	t.Parallel()
	out := Generate(GenerateInput{
		Soul: &types.SoulConfig{EmotiveStyle: types.EmotiveStyle{Humor: 7}},
	})
	contract := strings.Index(out, "How To Read This Prompt")
	principles := strings.Index(out, "Operating Principles")
	personality := strings.Index(out, "Personality & Style")
	switch {
	case contract < 0:
		t.Fatal("the prompt contract is missing from the assembled prompt")
	case principles < 0 || personality < 0:
		t.Fatal("expected sections are missing")
	case contract > principles, contract > personality:
		t.Error("the contract must precede the instructions it governs")
	}
	if !strings.Contains(out, "It is not a message") {
		t.Error("the contract must say the prompt is not a message from the user")
	}
}

func TestBuildSafetyMentionsUntrustedDelimiters(t *testing.T) {
	t.Parallel()
	s := BuildSafety(nil)
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
	first := BuildSafety(nil).Body
	second := BuildSafety(nil).Body
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

// The empty branch has to carry the SAME completeness assertion the
// populated one does, and this test replaced one that only checked for
// the string "(none installed)".
//
// That string was the whole body, and dropping the assertion is what
// left the model with nothing to anchor on: asked "what skills do you
// have", it answered out of the provider table instead. The assertion
// matters most when there is nothing to list.
func TestBuildSkillsEmptyStillClaimsCompleteness(t *testing.T) {
	t.Parallel()
	s := BuildSkills(nil, 0)
	body := strings.ToLower(s.Body)
	if !strings.Contains(body, "no skills are installed") {
		t.Errorf("empty skill list should say so plainly:\n%s", s.Body)
	}
	if !strings.Contains(body, "complete") {
		t.Errorf("empty branch dropped the completeness assertion:\n%s", s.Body)
	}
	if !strings.Contains(body, "provider") {
		t.Errorf("empty branch does not steer away from the provider list, which is "+
			"where the wrong answer came from:\n%s", s.Body)
	}
}

// A count of zero must produce no sentence at all. "0 proposals
// awaiting approval" and saying nothing look similar and are not: the
// provider is nil when self-learning is off, and a hardcoded zero
// would assert "none pending" for a feature that is not running.
func TestBuildSkillsSilentWhenNoProposals(t *testing.T) {
	t.Parallel()
	for _, s := range []Section{
		BuildSkills(nil, 0),
		BuildSkills([]SkillInfo{{Name: "alpha", Description: "a"}}, 0),
	} {
		if strings.Contains(strings.ToLower(s.Body), "approval") {
			t.Errorf("zero proposals should produce no approval line:\n%s", s.Body)
		}
	}
}

// Pending proposals are announced in both branches, and must never
// read as capability. They are inert: not materialised, invisible to
// the skill index, impossible to invoke.
func TestBuildSkillsAnnouncesProposalsWithoutOfferingThem(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]Section{
		"empty":     BuildSkills(nil, 2),
		"populated": BuildSkills([]SkillInfo{{Name: "alpha", Description: "a"}}, 2),
	} {
		body := s.Body
		if !strings.Contains(body, "2 self-taught proposals are awaiting") {
			t.Errorf("%s: proposal count not announced:\n%s", name, body)
		}
		if !strings.Contains(body, "NOT active") {
			t.Errorf("%s: a proposal must not read as available capability:\n%s", name, body)
		}
		if !strings.Contains(body, "learned_list") {
			t.Errorf("%s: announcing a count without naming the tool that shows them "+
				"is a tease:\n%s", name, body)
		}
	}
}

// Singular reads as singular. Small, and it is the difference between
// prose and a template somebody stopped reading.
func TestBuildSkillsProposalSingular(t *testing.T) {
	t.Parallel()
	body := BuildSkills(nil, 1).Body
	if !strings.Contains(body, "1 self-taught proposal is awaiting") {
		t.Errorf("singular proposal rendered as plural:\n%s", body)
	}
	// The first version got the verb right and the pronoun wrong —
	// "1 proposal is awaiting ... They are NOT active" — which is
	// exactly the half-pluralised sentence a single `noun` variable
	// produces. Both halves are checked so the next edit cannot fix
	// one and leave the other.
	if !strings.Contains(body, "It is NOT active") {
		t.Errorf("singular proposal described with a plural pronoun:\n%s", body)
	}
}

func TestBuildSkillsSortedByName(t *testing.T) {
	t.Parallel()
	skills := []SkillInfo{
		{Name: "writer", Description: "w", Location: "/opt/skills/writer"},
		{Name: "reader", Description: "r"},
	}
	s := BuildSkills(skills, 0)
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

func TestBuildWorkspaceEmptyPath(t *testing.T) {
	t.Parallel()
	s := BuildWorkspace("")
	if strings.Contains(s.Body, "/var/lobslaw/workspace") {
		t.Errorf("empty path should NOT emit a fabricated default; got %q", s.Body)
	}
	if !strings.Contains(s.Body, "No workspace mount is configured") {
		t.Errorf("empty path should surface 'not configured' message; got %q", s.Body)
	}
}

func TestBuildWorkspaceCustomPath(t *testing.T) {
	t.Parallel()
	s := BuildWorkspace("/app/data/workspace")
	if !strings.Contains(s.Body, "/app/data/workspace") {
		t.Errorf("custom path should render: %q", s.Body)
	}
}

// The assistant was asked whether it had self-learning enabled and
// said no, on a node where the review fork had proposed an artefact
// ten minutes earlier. Nothing in the prompt or the tool list
// mentioned it, so the honest answer was unavailable and a confident
// wrong one took its place.
func TestTheRuntimeSectionSaysWhetherSelfLearningIsOn(t *testing.T) {
	t.Parallel()
	body := BuildRuntime(RuntimeInfo{SelfLearning: "propose"}).Body
	if !strings.Contains(body, "self_learning: propose") {
		t.Errorf("mode missing from the runtime section:\n%s", body)
	}
	// The mode alone is a word; what matters to an answer is that
	// approval gates it.
	if !strings.Contains(body, "review queue") {
		t.Errorf("propose mode does not explain that approval gates it:\n%s", body)
	}
}

func TestAutoModeSaysItTakesEffectImmediately(t *testing.T) {
	t.Parallel()
	body := BuildRuntime(RuntimeInfo{SelfLearning: "auto"}).Body
	if !strings.Contains(body, "immediately") {
		t.Errorf("auto mode does not say artefacts apply at once:\n%s", body)
	}
}

// Off is the default, and a line claiming a capability the node does
// not have is worse than no line.
func TestSelfLearningIsUnmentionedWhenOff(t *testing.T) {
	t.Parallel()
	body := BuildRuntime(RuntimeInfo{Channel: "telegram", SelfLearning: ""}).Body
	if strings.Contains(body, "self_learning") {
		t.Errorf("a node with self-learning off advertises it:\n%s", body)
	}
}
