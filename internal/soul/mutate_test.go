package soul

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func newTestAdjuster(t *testing.T) (*Adjuster, string) {
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
	a, err := NewAdjuster(AdjusterConfig{Soul: s, Now: func() time.Time { return time.Now() }})
	if err != nil {
		t.Fatal(err)
	}
	return a, path
}

func TestSetNamePersists(t *testing.T) {
	t.Parallel()
	a, path := newTestAdjuster(t)
	got, err := a.SetName("Lobs")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Lobs" {
		t.Errorf("got %q", got)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "name: Lobs") {
		t.Errorf("name not persisted: %s", raw)
	}
	if !strings.Contains(string(raw), "freeform body here") {
		t.Error("body lost on rewrite")
	}
}

func TestTuneClampsToBaselineDrift(t *testing.T) {
	t.Parallel()
	a, _ := newTestAdjuster(t)
	// Push sarcasm to baseline+3 (max drift), then try +1 more — should refuse.
	for i := 0; i < 3; i++ {
		_, _, err := a.Tune("sarcasm", 1)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	prev, next, err := a.Tune("sarcasm", 1)
	if err == nil {
		t.Errorf("expected cap-reached error; got prev=%d next=%d", prev, next)
	}
}

func TestTuneRejectsUnknownDimension(t *testing.T) {
	t.Parallel()
	a, _ := newTestAdjuster(t)
	if _, _, err := a.Tune("arrogance", 1); err == nil {
		t.Error("expected error for unknown dimension")
	}
}

func TestAddFragmentSanitisesAndPersists(t *testing.T) {
	t.Parallel()
	a, path := newTestAdjuster(t)
	cleaned, total, err := a.AddFragment("  user supports `Liverpool` FC  ")
	if err != nil {
		t.Fatal(err)
	}
	if cleaned != "user supports Liverpool FC" {
		t.Errorf("cleaned = %q", cleaned)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "Liverpool") {
		t.Errorf("fragment not persisted: %s", raw)
	}
}

func TestAddFragmentRefusesDuplicates(t *testing.T) {
	t.Parallel()
	a, _ := newTestAdjuster(t)
	if _, _, err := a.AddFragment("likes coffee"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.AddFragment("likes coffee"); err == nil {
		t.Error("expected duplicate rejection")
	}
}

func TestAddFragmentRespectsCap(t *testing.T) {
	t.Parallel()
	a, _ := newTestAdjuster(t)
	for i := 0; i < MaxFragments; i++ {
		if _, _, err := a.AddFragment("fragment number " + strings.Repeat("x", i+1)); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if _, _, err := a.AddFragment("one too many"); err == nil {
		t.Error("expected cap to refuse extra fragment")
	}
}

func TestRemoveFragmentSubstring(t *testing.T) {
	t.Parallel()
	a, _ := newTestAdjuster(t)
	_, _, _ = a.AddFragment("user supports Liverpool FC")
	_, _, _ = a.AddFragment("prefers tea")
	removed, err := a.RemoveFragment("liverpool")
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
	a, path := newTestAdjuster(t)
	if _, err := a.SetName("Lobs"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetName("Bobs"); err != nil {
		t.Fatal(err)
	}
	// Two persists happened → one history entry exists (snapshotted
	// the original file before the first overwrite, then the
	// post-Lobs file before the Bobs overwrite). Step back 1 = "Lobs".
	ts, err := a.HistoryRollback(1)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if ts == "" {
		t.Error("expected timestamp")
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "name: Lobs") {
		t.Errorf("rollback didn't restore Lobs:\n%s", raw)
	}
}

func TestSetEmojiUsageRejectsInvalid(t *testing.T) {
	t.Parallel()
	a, _ := newTestAdjuster(t)
	if err := a.SetEmojiUsage("excessive"); err == nil {
		t.Error("expected rejection for invalid emoji_usage")
	}
	if err := a.SetEmojiUsage("generous"); err != nil {
		t.Errorf("valid value rejected: %v", err)
	}
}

// keep types package used so TestX cases that reference it compile
var _ = types.SoulConfig{}
