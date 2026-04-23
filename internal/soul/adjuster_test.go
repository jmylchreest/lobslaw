package soul

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// staticClassifier returns a pre-canned Feedback so tests isolate
// the adjuster's apply/persist/cap logic from the classifier.
type staticClassifier struct {
	fb  *Feedback
	err error
}

func (s staticClassifier) Classify(_ context.Context, _ string) (*Feedback, error) {
	return s.fb, s.err
}

func freshSoul(t *testing.T) *Soul {
	t.Helper()
	path := writeSoul(t, t.TempDir(), validSoul)
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAdjusterDecreasesSarcasm(t *testing.T) {
	t.Parallel()
	s := freshSoul(t)
	a, _ := NewAdjuster(AdjusterConfig{
		Soul: s,
		Classifier: staticClassifier{fb: &Feedback{
			Dimension: "sarcasm", Direction: DirectionDecrease,
		}},
	})

	before := s.Config.EmotiveStyle.Sarcasm
	res, err := a.Apply(context.Background(), "don't be so snarky")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied {
		t.Errorf("expected Applied; reason: %q", res.Reason)
	}
	if res.NewValue >= before {
		t.Errorf("sarcasm should have decreased; %d → %d", before, res.NewValue)
	}
}

func TestAdjusterPersistsToDisk(t *testing.T) {
	t.Parallel()
	s := freshSoul(t)
	a, _ := NewAdjuster(AdjusterConfig{
		Soul: s,
		Classifier: staticClassifier{fb: &Feedback{
			Dimension: "sarcasm", Direction: DirectionDecrease,
		}},
	})
	if _, err := a.Apply(context.Background(), "less snark"); err != nil {
		t.Fatal(err)
	}

	// Reload from disk. The sarcasm value must reflect the apply.
	reloaded, err := Load(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Config.EmotiveStyle.Sarcasm >= 2 {
		// Baseline from validSoul is 2; a decrease by coef=0.15*10≈2
		// rounds to −2. Sarcasm becomes 0.
		t.Errorf("on-disk sarcasm should have decreased from 2; got %d",
			reloaded.Config.EmotiveStyle.Sarcasm)
	}
}

func TestAdjusterPreservesBody(t *testing.T) {
	t.Parallel()
	s := freshSoul(t)
	originalBody := s.Body
	a, _ := NewAdjuster(AdjusterConfig{
		Soul: s,
		Classifier: staticClassifier{fb: &Feedback{
			Dimension: "sarcasm", Direction: DirectionDecrease,
		}},
	})
	if _, err := a.Apply(context.Background(), "less snark"); err != nil {
		t.Fatal(err)
	}

	reloaded, _ := Load(s.Path)
	if !strings.Contains(reloaded.Body, "freeform notes") {
		t.Errorf("body was lost on persist; got %q", reloaded.Body)
	}
	if len(reloaded.Body) == 0 && len(originalBody) > 0 {
		t.Error("body emptied out")
	}
}

func TestAdjusterCooldownBlocksSameDimension(t *testing.T) {
	t.Parallel()
	s := freshSoul(t)
	t0 := time.Unix(1_700_000_000, 0)
	now := t0
	a, _ := NewAdjuster(AdjusterConfig{
		Soul: s,
		Classifier: staticClassifier{fb: &Feedback{
			Dimension: "sarcasm", Direction: DirectionDecrease,
		}},
		Now: func() time.Time { return now },
	})

	if _, err := a.Apply(context.Background(), "less snark"); err != nil {
		t.Fatal(err)
	}
	// Advance clock by 1 hour — less than the 24h cooldown.
	now = t0.Add(time.Hour)
	res, _ := a.Apply(context.Background(), "less snark again")
	if res.Applied {
		t.Error("second apply within cooldown should NOT take effect")
	}
	if !strings.Contains(res.Reason, "cooling down") {
		t.Errorf("reason should mention cooldown; got %q", res.Reason)
	}
}

func TestAdjusterCooldownExpires(t *testing.T) {
	t.Parallel()
	// Start with baseline=8 so two consecutive decreases stay
	// within the ±3 cap from baseline (8 → 6 → 5 are all legal).
	path := writeSoul(t, t.TempDir(), strings.Replace(validSoul, "sarcasm: 2", "sarcasm: 8", 1))
	s, _ := Load(path)
	t0 := time.Unix(1_700_000_000, 0)
	now := t0
	a, _ := NewAdjuster(AdjusterConfig{
		Soul: s,
		Classifier: staticClassifier{fb: &Feedback{
			Dimension: "sarcasm", Direction: DirectionDecrease,
		}},
		Now: func() time.Time { return now },
	})
	_, _ = a.Apply(context.Background(), "less snark")
	// Advance PAST the cooldown window.
	now = t0.Add(25 * time.Hour)
	res, _ := a.Apply(context.Background(), "less snark still")
	if !res.Applied {
		t.Errorf("apply after cooldown expired should succeed; reason: %q", res.Reason)
	}
}

// TestAdjusterCapsAtBaselinePlus3 — walking decrease calls past the
// ±3 cap from baseline must stop at the cap, not trend to zero.
func TestAdjusterCapsAtBaselinePlus3(t *testing.T) {
	t.Parallel()
	// Use a higher baseline so the cap is visible without hitting 0.
	path := writeSoul(t, t.TempDir(), strings.Replace(validSoul, "sarcasm: 2", "sarcasm: 8", 1))
	s, _ := Load(path)
	t0 := time.Unix(1_700_000_000, 0)
	now := t0
	a, _ := NewAdjuster(AdjusterConfig{
		Soul: s,
		Classifier: staticClassifier{fb: &Feedback{
			Dimension: "sarcasm", Direction: DirectionDecrease,
		}},
		Now: func() time.Time { return now },
	})
	// Hammer decreases with cooldown skipped by advancing the clock.
	for range 10 {
		_, _ = a.Apply(context.Background(), "less snark")
		now = now.Add(48 * time.Hour)
	}
	got := a.Soul().Config.EmotiveStyle.Sarcasm
	// baseline 8, cap −3 = 5. Can't go lower regardless of attempts.
	if got < 5 {
		t.Errorf("sarcasm drifted below baseline-3 cap: baseline=8 got=%d", got)
	}
}

func TestAdjusterUnclassifiableGivesReason(t *testing.T) {
	t.Parallel()
	s := freshSoul(t)
	a, _ := NewAdjuster(AdjusterConfig{
		Soul:       s,
		Classifier: staticClassifier{err: ErrNoClassification},
	})
	res, err := a.Apply(context.Background(), "what's the weather")
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied {
		t.Error("unclassifiable feedback should not apply")
	}
	if !strings.Contains(res.Reason, "classify") {
		t.Errorf("reason should mention classification: %q", res.Reason)
	}
}

func TestAdjusterCooldownRemaining(t *testing.T) {
	t.Parallel()
	s := freshSoul(t)
	t0 := time.Unix(1_700_000_000, 0)
	now := t0
	a, _ := NewAdjuster(AdjusterConfig{
		Soul: s,
		Classifier: staticClassifier{fb: &Feedback{
			Dimension: "sarcasm", Direction: DirectionDecrease,
		}},
		Now: func() time.Time { return now },
	})

	if got := a.CooldownRemaining("sarcasm"); got != 0 {
		t.Errorf("pre-apply remaining should be 0; got %v", got)
	}

	_, _ = a.Apply(context.Background(), "less snark")
	now = t0.Add(2 * time.Hour)
	got := a.CooldownRemaining("sarcasm")
	// 24h - 2h = 22h.
	if got < 21*time.Hour || got > 23*time.Hour {
		t.Errorf("remaining should be ~22h; got %v", got)
	}
}

// TestAdjusterInMemoryOnly — DefaultSoul has no Path; Apply mutates
// in-memory state but persistLocked must not fail when there's
// nothing to persist.
func TestAdjusterInMemoryOnly(t *testing.T) {
	t.Parallel()
	s := DefaultSoul() // Path is empty
	// DefaultSoul doesn't set a baseline sarcasm; the test doesn't
	// care about numeric cap, only that Apply doesn't try to write.
	a, _ := NewAdjuster(AdjusterConfig{
		Soul: s,
		Classifier: staticClassifier{fb: &Feedback{
			Dimension: "sarcasm", Direction: DirectionIncrease,
		}},
	})
	res, err := a.Apply(context.Background(), "more snark")
	if err != nil {
		t.Fatalf("persistLocked with empty Path must not error: %v", err)
	}
	if !res.Applied {
		t.Errorf("in-memory adjustment should still Apply=true; reason: %q", res.Reason)
	}
}

func TestAdjusterNilSoul(t *testing.T) {
	t.Parallel()
	_, err := NewAdjuster(AdjusterConfig{})
	if err == nil {
		t.Error("nil Soul should fail construction")
	}
}

// TestEncodeFrontmatterRoundTrip — frontmatter serialisation is
// stable enough that a Parse → encode cycle produces a semantically
// identical SoulConfig. Guards against "adjustments silently drop
// a field on every persist."
func TestEncodeFrontmatterRoundTrip(t *testing.T) {
	t.Parallel()
	path := writeSoul(t, t.TempDir(), validSoul)
	original, _ := Load(path)

	encoded, err := encodeFrontmatter(original.Config)
	if err != nil {
		t.Fatal(err)
	}
	// Write it back + reload.
	full := "---\n" + string(encoded) + "---\n\n" + original.Body + "\n"
	newPath := filepath.Join(t.TempDir(), "SOUL.md")
	_ = os.WriteFile(newPath, []byte(full), 0o644)

	reloaded, err := Load(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Config.Name != original.Config.Name {
		t.Errorf("name lost: %q vs %q", reloaded.Config.Name, original.Config.Name)
	}
	if reloaded.Config.EmotiveStyle.Excitement != original.Config.EmotiveStyle.Excitement {
		t.Errorf("excitement lost: %d vs %d",
			reloaded.Config.EmotiveStyle.Excitement, original.Config.EmotiveStyle.Excitement)
	}
	if reloaded.Config.Adjustments.CooldownPeriod != original.Config.Adjustments.CooldownPeriod {
		t.Errorf("cooldown lost: %v vs %v",
			reloaded.Config.Adjustments.CooldownPeriod, original.Config.Adjustments.CooldownPeriod)
	}
}
