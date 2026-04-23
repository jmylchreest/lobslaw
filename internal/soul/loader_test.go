package soul

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSoul(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "SOUL.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const validSoul = `---
name: buddy
scope: default
culture: professional
nationality: british
language:
  default: en
  detect: true
persona_description: a helpful assistant
emotive_style:
  emoji_usage: minimal
  excitement: 5
  formality: 5
  directness: 7
  sarcasm: 2
  humor: 3
adjustments:
  feedback_coefficient: 0.15
  cooldown_period: 24h
feedback:
  classifier: llm
---

# Body

some freeform notes.
`

func TestLoadHappyPath(t *testing.T) {
	t.Parallel()
	path := writeSoul(t, t.TempDir(), validSoul)
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Config.Name != "buddy" {
		t.Errorf("name: %q", s.Config.Name)
	}
	if s.Config.Nationality != "british" {
		t.Errorf("nationality: %q", s.Config.Nationality)
	}
	if s.Config.EmotiveStyle.Excitement != 5 {
		t.Errorf("excitement: %d", s.Config.EmotiveStyle.Excitement)
	}
	if s.Config.Adjustments.CooldownPeriod != 24*time.Hour {
		t.Errorf("cooldown_period: %v", s.Config.Adjustments.CooldownPeriod)
	}
	if !strings.Contains(s.Body, "freeform notes") {
		t.Errorf("body not captured: %q", s.Body)
	}
	if s.Path == "" {
		t.Error("Path should be populated after Load")
	}
}

func TestLoadReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	_, err := Load(filepath.Join(t.TempDir(), "nope.md"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound; got %v", err)
	}
}

func TestLoadOrDefaultFallsBackOnMissing(t *testing.T) {
	t.Parallel()
	s, err := LoadOrDefault(filepath.Join(t.TempDir(), "nope.md"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Config.Name != "assistant" {
		t.Errorf("default name: %q", s.Config.Name)
	}
	// Defaults should still pass validate.
	if s.Config.EmotiveStyle.EmojiUsage != "minimal" {
		t.Errorf("default emoji_usage: %q", s.Config.EmotiveStyle.EmojiUsage)
	}
}

func TestLoadOrDefaultPropagatesRealError(t *testing.T) {
	t.Parallel()
	path := writeSoul(t, t.TempDir(), `---
emotive_style:
  excitement: 99
---`)
	_, err := LoadOrDefault(path)
	if err == nil {
		t.Fatal("malformed SOUL should propagate, not silently use defaults")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("validation errors should NOT map to ErrNotFound")
	}
}

func TestParseRejectsOutOfRangeDimension(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`---
emotive_style:
  excitement: 15
---`), "")
	if err == nil || !strings.Contains(err.Error(), "0–10") {
		t.Errorf("excitement=15 should be rejected; got %v", err)
	}
}

func TestParseRejectsUnknownEmojiUsage(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`---
emotive_style:
  emoji_usage: extreme
---`), "")
	if err == nil || !strings.Contains(err.Error(), "emoji_usage") {
		t.Errorf("emoji_usage=extreme should be rejected; got %v", err)
	}
}

func TestParseRejectsUnknownClassifier(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`---
feedback:
  classifier: astrology
---`), "")
	if err == nil || !strings.Contains(err.Error(), "classifier") {
		t.Errorf("unknown classifier should be rejected; got %v", err)
	}
}

// TestParseMissingFrontmatterTreatsFileAsBody — a SOUL.md without
// "---" frontmatter markers parses as default config + the whole
// file treated as markdown body. Lets operators ship a minimal
// SOUL.md with just prose context.
func TestParseMissingFrontmatterTreatsFileAsBody(t *testing.T) {
	t.Parallel()
	s, err := Parse([]byte("just some notes here"), "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Body != "just some notes here" {
		t.Errorf("body: %q", s.Body)
	}
	if s.Config.Scope != "default" {
		t.Errorf("default scope missing: %q", s.Config.Scope)
	}
}

func TestParseUnclosedFrontmatterErrors(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte("---\nname: x\n"), "")
	if err == nil || !strings.Contains(err.Error(), "closing") {
		t.Errorf("unclosed frontmatter should error; got %v", err)
	}
}

func TestParseFrontmatterOnlyNoBody(t *testing.T) {
	t.Parallel()
	s, err := Parse([]byte("---\nname: solo\n---\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Config.Name != "solo" {
		t.Errorf("name: %q", s.Config.Name)
	}
	if s.Body != "" {
		t.Errorf("body should be empty; got %q", s.Body)
	}
}

// TestParseCRLFNormalisation — files edited on Windows arrive with
// CRLF line endings. The parser must tolerate them without the
// frontmatter regex missing the closing "---".
func TestParseCRLFNormalisation(t *testing.T) {
	t.Parallel()
	raw := []byte("---\r\nname: crlf\r\n---\r\nbody\r\n")
	s, err := Parse(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Config.Name != "crlf" {
		t.Errorf("name: %q", s.Config.Name)
	}
	if s.Body != "body" {
		t.Errorf("body: %q", s.Body)
	}
}

func TestApplyDefaultsFillsInOmittedFields(t *testing.T) {
	t.Parallel()
	s, err := Parse([]byte(`---
name: bare
emotive_style:
  emoji_usage: minimal
---`), "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Config.Scope != "default" {
		t.Errorf("default scope missing: %q", s.Config.Scope)
	}
	if s.Config.Language.Default != "en" {
		t.Errorf("default language: %q", s.Config.Language.Default)
	}
	if s.Config.Adjustments.FeedbackCoefficient != 0.15 {
		t.Errorf("default feedback_coefficient: %v", s.Config.Adjustments.FeedbackCoefficient)
	}
	if s.Config.Feedback.Classifier != "llm" {
		t.Errorf("default classifier: %q", s.Config.Feedback.Classifier)
	}
}

// TestDefaultSoulValidates — DefaultSoul's output must pass
// validate(), otherwise LoadOrDefault returns a corrupt soul on
// missing-file fallback.
func TestDefaultSoulValidates(t *testing.T) {
	t.Parallel()
	s := DefaultSoul()
	if err := validate(&s.Config); err != nil {
		t.Errorf("DefaultSoul doesn't validate: %v", err)
	}
}
