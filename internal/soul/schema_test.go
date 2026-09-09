package soul

import (
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/promptgen"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestVersionedSoulStyle(t *testing.T) {
	s, err := Parse([]byte("---\nschema_version: 1\nverbosity: concise\nlanguage:\n  spelling_locale: en-GB\nemotive_style:\n  sarcasm: 0\nadjustments:\n  feedback_coefficient: 0\n  cooldown_period: 0s\n---\nKeep style silent."), "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Config.EmotiveStyle.Formality != types.SoulNeutralScore || s.Config.EmotiveStyle.Sarcasm != 0 {
		t.Fatalf("omitted dials must default to neutral while explicit zero is preserved: %+v", s.Config.EmotiveStyle)
	}
	if s.Config.Adjustments.FeedbackCoefficient != 0 || s.Config.Adjustments.CooldownPeriod != 0 {
		t.Fatal("explicit zero replaced by default")
	}
	body := promptgen.BuildPersonality(&s.Config, nil).Body
	for _, want := range []string{"No sarcasm.", "Keep replies concise", "en-GB"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	legacy, err := Parse([]byte("---\nemotive_style:\n  sarcasm: 0\n---\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(promptgen.BuildPersonality(&legacy.Config, nil).Body, "No sarcasm.") {
		t.Fatal("legacy zero semantics changed")
	}
}

func TestVersionedSoulRejectsInvalidConfiguration(t *testing.T) {
	for _, fields := range []string{
		"schema_version: 9", "schema_version: -1",
		"schema_version: 1\nverbositty: concise",
		"schema_version: 1\nemotive_style:\n  humorr: 2",
		"schema_version: 1\nverbosity: tiny",
		"schema_version: 1\nlanguage:\n  spelling_locale: invalid_locale_!",
		"schema_version: 1\nadjustments:\n  feedback_coefficient: -0.1",
		"schema_version: 1\nadjustments:\n  feedback_coefficient: .nan",
		"schema_version: 1\nadjustments:\n  feedback_coefficient: 1.1",
		"schema_version: 1\nadjustments:\n  cooldown_period: -1s",
	} {
		t.Run(fields, func(t *testing.T) {
			if _, err := Parse([]byte("---\n"+fields+"\n---\n"), ""); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
