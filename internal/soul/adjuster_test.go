package soul

import (
	"context"
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

// newAdjuster wires an Adjuster with an in-process MemoryTuneStore
// for tests. Production wiring uses the raft-backed adapter.
func newAdjuster(t *testing.T, cfg AdjusterConfig) *Adjuster {
	t.Helper()
	if cfg.Store == nil {
		cfg.Store = NewMemoryTuneStore()
	}
	a, err := NewAdjuster(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAdjusterDecreasesSarcasm(t *testing.T) {
	t.Parallel()
	s := freshSoul(t)
	a := newAdjuster(t, AdjusterConfig{
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

// TestAdjusterPersistsViaStore — the Apply path must round-trip
// through the TuneStore so cluster nodes converge on the same tune.
func TestAdjusterPersistsViaStore(t *testing.T) {
	t.Parallel()
	s := freshSoul(t)
	store := NewMemoryTuneStore()
	a := newAdjuster(t, AdjusterConfig{
		Soul:  s,
		Store: store,
		Classifier: staticClassifier{fb: &Feedback{
			Dimension: "sarcasm", Direction: DirectionDecrease,
		}},
	})
	if _, err := a.Apply(context.Background(), "less snark"); err != nil {
		t.Fatal(err)
	}

	persisted, err := store.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if persisted == nil || persisted.Sarcasm == nil {
		t.Fatalf("store should have a sarcasm override after Apply; got %+v", persisted)
	}
	if *persisted.Sarcasm >= s.Config.EmotiveStyle.Sarcasm {
		t.Errorf("persisted sarcasm should be below baseline %d; got %d",
			s.Config.EmotiveStyle.Sarcasm, *persisted.Sarcasm)
	}
}

// TestAdjusterMergesBaselineWithTune — only fields the agent has
// explicitly tuned override baseline; untuned dimensions follow
// baseline so an operator edit to SOUL.md propagates correctly.
func TestAdjusterMergesBaselineWithTune(t *testing.T) {
	t.Parallel()
	s := freshSoul(t)
	store := NewMemoryTuneStore()
	a := newAdjuster(t, AdjusterConfig{
		Soul:  s,
		Store: store,
		Classifier: staticClassifier{fb: &Feedback{
			Dimension: "sarcasm", Direction: DirectionDecrease,
		}},
	})
	_, _ = a.Apply(context.Background(), "less snark")

	merged := a.Soul()
	if merged.Config.EmotiveStyle.Excitement != s.Config.EmotiveStyle.Excitement {
		t.Errorf("untuned dimension should follow baseline: excitement %d vs %d",
			merged.Config.EmotiveStyle.Excitement, s.Config.EmotiveStyle.Excitement)
	}
	if merged.Config.EmotiveStyle.Sarcasm == s.Config.EmotiveStyle.Sarcasm {
		t.Errorf("tuned sarcasm should differ from baseline %d; got %d",
			s.Config.EmotiveStyle.Sarcasm, merged.Config.EmotiveStyle.Sarcasm)
	}
}

func TestAdjusterCooldownBlocksSameDimension(t *testing.T) {
	t.Parallel()
	s := freshSoul(t)
	t0 := time.Unix(1_700_000_000, 0)
	now := t0
	a := newAdjuster(t, AdjusterConfig{
		Soul: s,
		Classifier: staticClassifier{fb: &Feedback{
			Dimension: "sarcasm", Direction: DirectionDecrease,
		}},
		Now: func() time.Time { return now },
	})

	if _, err := a.Apply(context.Background(), "less snark"); err != nil {
		t.Fatal(err)
	}
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
	path := writeSoul(t, t.TempDir(), strings.Replace(validSoul, "sarcasm: 2", "sarcasm: 8", 1))
	s, _ := Load(path)
	t0 := time.Unix(1_700_000_000, 0)
	now := t0
	a := newAdjuster(t, AdjusterConfig{
		Soul: s,
		Classifier: staticClassifier{fb: &Feedback{
			Dimension: "sarcasm", Direction: DirectionDecrease,
		}},
		Now: func() time.Time { return now },
	})
	_, _ = a.Apply(context.Background(), "less snark")
	now = t0.Add(25 * time.Hour)
	res, _ := a.Apply(context.Background(), "less snark still")
	if !res.Applied {
		t.Errorf("apply after cooldown expired should succeed; reason: %q", res.Reason)
	}
}

func TestAdjusterCapsAtBaselinePlus3(t *testing.T) {
	t.Parallel()
	path := writeSoul(t, t.TempDir(), strings.Replace(validSoul, "sarcasm: 2", "sarcasm: 8", 1))
	s, _ := Load(path)
	t0 := time.Unix(1_700_000_000, 0)
	now := t0
	a := newAdjuster(t, AdjusterConfig{
		Soul: s,
		Classifier: staticClassifier{fb: &Feedback{
			Dimension: "sarcasm", Direction: DirectionDecrease,
		}},
		Now: func() time.Time { return now },
	})
	for range 10 {
		_, _ = a.Apply(context.Background(), "less snark")
		now = now.Add(48 * time.Hour)
	}
	got := a.Soul().Config.EmotiveStyle.Sarcasm
	if got < 5 {
		t.Errorf("sarcasm drifted below baseline-3 cap: baseline=8 got=%d", got)
	}
}

func TestAdjusterUnclassifiableGivesReason(t *testing.T) {
	t.Parallel()
	s := freshSoul(t)
	a := newAdjuster(t, AdjusterConfig{
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
	a := newAdjuster(t, AdjusterConfig{
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
	if got < 21*time.Hour || got > 23*time.Hour {
		t.Errorf("remaining should be ~22h; got %v", got)
	}
}

// TestAdjusterDefaultSoul — DefaultSoul has no on-disk file and a
// minimal config. Apply still works because mutations go through
// the TuneStore, not the file path.
func TestAdjusterDefaultSoul(t *testing.T) {
	t.Parallel()
	s := DefaultSoul()
	a := newAdjuster(t, AdjusterConfig{
		Soul: s,
		Classifier: staticClassifier{fb: &Feedback{
			Dimension: "sarcasm", Direction: DirectionIncrease,
		}},
	})
	res, err := a.Apply(context.Background(), "more snark")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied {
		t.Errorf("default-soul adjustment should still Apply=true; reason: %q", res.Reason)
	}
}

func TestAdjusterNilSoul(t *testing.T) {
	t.Parallel()
	_, err := NewAdjuster(AdjusterConfig{Store: NewMemoryTuneStore()})
	if err == nil {
		t.Error("nil Soul should fail construction")
	}
}

func TestAdjusterNilStore(t *testing.T) {
	t.Parallel()
	_, err := NewAdjuster(AdjusterConfig{Soul: DefaultSoul()})
	if err == nil {
		t.Error("nil Store should fail construction")
	}
}
