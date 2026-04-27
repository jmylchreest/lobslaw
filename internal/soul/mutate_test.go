package soul

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func newTestAdjuster(t *testing.T) (*Adjuster, *MemoryTuneStore) {
	t.Helper()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "SOUL.md")
	body := `---
name: bot
scope: default
persona_description: a test bot
emotive_style:
  emoji_usage: minimal
  excitement: 5
  formality: 5
  directness: 5
  sarcasm: 5
  humor: 5
adjustments:
  feedback_coefficient: 0.15
feedback:
  classifier: regex
---

freeform body here
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryTuneStore()
	a, err := NewAdjuster(AdjusterConfig{Soul: s, Store: store, Now: func() time.Time { return time.Now() }})
	if err != nil {
		t.Fatal(err)
	}
	return a, store
}

func TestSetNamePersistsToStore(t *testing.T) {
	t.Parallel()
	a, store := newTestAdjuster(t)
	got, err := a.SetName(context.Background(), "Lobs")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Lobs" {
		t.Errorf("got %q", got)
	}
	persisted, err := store.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if persisted == nil || persisted.Name == nil || *persisted.Name != "Lobs" {
		t.Errorf("name not persisted to store: %+v", persisted)
	}
	// Soul() should reflect the override; baseline body unchanged.
	if a.Soul().Config.Name != "Lobs" {
		t.Errorf("Soul() didn't reflect SetName: %+v", a.Soul().Config)
	}
}

func TestTuneClampsToBaselineDrift(t *testing.T) {
	t.Parallel()
	a, _ := newTestAdjuster(t)
	for i := 0; i < 3; i++ {
		_, _, err := a.Tune(context.Background(), "sarcasm", 1)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	prev, next, err := a.Tune(context.Background(), "sarcasm", 1)
	if err == nil {
		t.Errorf("expected cap-reached error; got prev=%d next=%d", prev, next)
	}
}

func TestTuneRejectsUnknownDimension(t *testing.T) {
	t.Parallel()
	a, _ := newTestAdjuster(t)
	if _, _, err := a.Tune(context.Background(), "arrogance", 1); err == nil {
		t.Error("expected error for unknown dimension")
	}
}

func TestAddFragmentSanitisesAndPersists(t *testing.T) {
	t.Parallel()
	a, store := newTestAdjuster(t)
	cleaned, total, err := a.AddFragment(context.Background(), "  user supports `Liverpool` FC  ")
	if err != nil {
		t.Fatal(err)
	}
	if cleaned != "user supports Liverpool FC" {
		t.Errorf("cleaned = %q", cleaned)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	persisted, _ := store.Get(context.Background())
	if persisted == nil || persisted.Fragments == nil || len(*persisted.Fragments) != 1 {
		t.Errorf("fragment not persisted: %+v", persisted)
	}
}

func TestAddFragmentRefusesDuplicates(t *testing.T) {
	t.Parallel()
	a, _ := newTestAdjuster(t)
	if _, _, err := a.AddFragment(context.Background(), "likes coffee"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.AddFragment(context.Background(), "likes coffee"); err == nil {
		t.Error("expected duplicate rejection")
	}
}

func TestAddFragmentRespectsCap(t *testing.T) {
	t.Parallel()
	a, _ := newTestAdjuster(t)
	for i := 0; i < MaxFragments; i++ {
		if _, _, err := a.AddFragment(context.Background(), "fragment number "+strings.Repeat("x", i+1)); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if _, _, err := a.AddFragment(context.Background(), "one too many"); err == nil {
		t.Error("expected cap to refuse extra fragment")
	}
}

func TestRemoveFragmentSubstring(t *testing.T) {
	t.Parallel()
	a, _ := newTestAdjuster(t)
	_, _, _ = a.AddFragment(context.Background(), "user supports Liverpool FC")
	_, _, _ = a.AddFragment(context.Background(), "prefers tea")
	removed, err := a.RemoveFragment(context.Background(), "liverpool")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(removed, "Liverpool") {
		t.Errorf("removed = %q", removed)
	}
	if got := a.ListFragments(); len(got) != 1 || got[0] != "prefers tea" {
		t.Errorf("post-remove fragments wrong: %v", got)
	}
}

func TestHistoryRollbackRestoresPriorVersion(t *testing.T) {
	t.Parallel()
	a, _ := newTestAdjuster(t)
	if _, err := a.SetName(context.Background(), "Lobs"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetName(context.Background(), "Bobs"); err != nil {
		t.Fatal(err)
	}
	// Two persists happened. Step back 1 → restore the state before
	// the most recent Put, which is the "Lobs" state.
	if _, err := a.HistoryRollback(context.Background(), 1); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := a.Soul().Config.Name; got != "Lobs" {
		t.Errorf("rollback didn't restore Lobs: got %q", got)
	}
}

func TestSetEmojiUsageRejectsInvalid(t *testing.T) {
	t.Parallel()
	a, _ := newTestAdjuster(t)
	if err := a.SetEmojiUsage(context.Background(), "excessive"); err == nil {
		t.Error("expected rejection for invalid emoji_usage")
	}
	if err := a.SetEmojiUsage(context.Background(), "generous"); err != nil {
		t.Errorf("valid value rejected: %v", err)
	}
}

// keep types package used so TestX cases that reference it compile
var _ = types.SoulConfig{}
