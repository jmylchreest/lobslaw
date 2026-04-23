package soul

import (
	"testing"

	"github.com/pemistahl/lingua-go"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Short samples are deliberately on the "will this work?" edge —
// lingua on a one-word sample is often ambiguous. Use sentences
// long enough that the n-gram model has something to go on.
func TestLinguaDetectorEnglish(t *testing.T) {
	t.Parallel()
	d := NewLinguaDetector()
	got := d.Detect("Hello there, I am writing a short test message in English.")
	if got != "en" {
		t.Errorf("expected en; got %q", got)
	}
}

func TestLinguaDetectorGerman(t *testing.T) {
	t.Parallel()
	d := NewLinguaDetector()
	got := d.Detect("Guten Morgen, ich schreibe eine kurze Nachricht auf Deutsch.")
	if got != "de" {
		t.Errorf("expected de; got %q", got)
	}
}

func TestLinguaDetectorFrench(t *testing.T) {
	t.Parallel()
	d := NewLinguaDetector()
	got := d.Detect("Bonjour, j'écris un petit message en français pour tester.")
	if got != "fr" {
		t.Errorf("expected fr; got %q", got)
	}
}

func TestLinguaDetectorEmptySample(t *testing.T) {
	t.Parallel()
	d := NewLinguaDetector()
	if got := d.Detect(""); got != "" {
		t.Errorf("empty sample should produce empty result; got %q", got)
	}
	if got := d.Detect("   "); got != "" {
		t.Errorf("whitespace-only sample should produce empty result; got %q", got)
	}
}

// TestLinguaDetectorPreloadConstraint — a detector preloaded with
// English + Spanish can't return "de" for a German sample. Proves
// FromLanguages narrowing actually takes effect. (Lingua requires
// at least 2 preloaded languages; a single-language builder panics.)
func TestLinguaDetectorPreloadConstraint(t *testing.T) {
	t.Parallel()
	d := NewLinguaDetectorWith(lingua.English, lingua.Spanish)
	got := d.Detect("Guten Morgen, ich schreibe eine Nachricht auf Deutsch.")
	if got == "de" {
		t.Error("detector constrained to English+Spanish shouldn't report German")
	}
}

// TestLinguaDetectorOnceConstructsOnce — two calls share the
// same internal detector instance (verified by the once.Do contract;
// can't directly assert without exposing internals, so we test the
// behavioural outcome: both calls return the same result).
func TestLinguaDetectorOnceConstructsOnce(t *testing.T) {
	t.Parallel()
	d := NewLinguaDetector()
	first := d.Detect("This is an English sentence, long enough to classify.")
	second := d.Detect("This is an English sentence, long enough to classify.")
	if first != second {
		t.Errorf("two calls produced different results: %q vs %q", first, second)
	}
}

func TestNullDetectorAlwaysEmpty(t *testing.T) {
	t.Parallel()
	var d Detector = NullDetector{}
	for _, s := range []string{"", "hello", "Guten Morgen", "你好"} {
		if got := d.Detect(s); got != "" {
			t.Errorf("NullDetector(%q) = %q; want empty", s, got)
		}
	}
}

// TestNewDetectorRespectsConfig — a Soul with language.detect=false
// returns NullDetector; detect=true returns a real LinguaDetector.
// Callers don't have to duplicate the on/off branch.
func TestNewDetectorRespectsConfig(t *testing.T) {
	t.Parallel()
	off := Soul{Config: types.SoulConfig{
		Language: types.Language{Detect: false, Default: "en"},
	}}
	if _, ok := NewDetector(off).(NullDetector); !ok {
		t.Error("detect=false should return NullDetector")
	}

	on := Soul{Config: types.SoulConfig{
		Language: types.Language{Detect: true, Default: "en"},
	}}
	if _, ok := NewDetector(on).(*LinguaDetector); !ok {
		t.Error("detect=true should return *LinguaDetector")
	}
}
