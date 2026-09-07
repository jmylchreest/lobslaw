package soul

import (
	"context"
	"sync"
	"testing"
)

func TestReloadPreservesOverridesAndResetInherits(t *testing.T) {
	a, _ := newTestAdjuster(t)
	ctx := context.Background()
	if _, _, err := a.Tune(ctx, "sarcasm", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetName(ctx, "tuned"); err != nil {
		t.Fatal(err)
	}
	baseline := freshSoul(t)
	baseline.Config.Name = "replacement"
	baseline.Config.EmotiveStyle.Sarcasm = 1
	baseline.Config.EmotiveStyle.Humor = 9
	baseline.Body = "Use short sentences."
	a.ReplaceBaseline(baseline)
	s := a.Soul()
	if s.Config.Name != "tuned" || s.Config.EmotiveStyle.Sarcasm != 4 || s.Config.EmotiveStyle.Humor != 9 || s.Body != baseline.Body {
		t.Fatalf("incorrect merged state: %+v", s)
	}
	if err := a.Reset(ctx, "name"); err != nil {
		t.Fatal(err)
	}
	if a.Soul().Config.Name != "replacement" {
		t.Fatal("reset did not inherit baseline")
	}
	if err := a.Reset(ctx, "all"); err != nil {
		t.Fatal(err)
	}
	if len(a.Soul().Overrides) != 0 || a.Soul().Config.EmotiveStyle.Sarcasm != 1 {
		t.Fatal("all overrides were not cleared")
	}
}

func TestRollbackFirstEditRestoresInheritance(t *testing.T) {
	a, _ := newTestAdjuster(t)
	ctx := context.Background()
	before := a.Soul().Config.Name
	if _, err := a.SetName(ctx, "changed"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.HistoryRollback(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if a.Soul().Config.Name != before {
		t.Fatal("first edit was not undone")
	}
}

func TestConcurrentReloadTuneRefresh(t *testing.T) {
	a, _ := newTestAdjuster(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			b := freshSoul(t)
			b.Config.EmotiveStyle.Sarcasm = i % 10
			a.ReplaceBaseline(b)
			_, _, _ = a.Tune(ctx, "sarcasm", 1)
			if err := a.RefreshTune(ctx); err != nil {
				t.Error(err)
			}
			_ = a.Soul()
		})
	}
	wg.Wait()
	if _, err := a.SetName(ctx, "last write"); err != nil {
		t.Fatal(err)
	}
	if err := a.RefreshTune(ctx); err != nil {
		t.Fatal(err)
	}
	if a.Soul().Config.Name != "last write" {
		t.Fatal("refresh lost the latest write")
	}
}
