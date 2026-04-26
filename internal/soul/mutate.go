package soul

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// HistoryDirName is the subdirectory next to SOUL.md where prior
// versions are kept. Created lazily on first persist.
const HistoryDirName = "soul.d/history"

// MaxHistoryVersions caps how many timestamped snapshots are kept.
// Older entries get pruned at write time so the directory doesn't
// grow unbounded across years of self-tuning.
const MaxHistoryVersions = 20

// SetName replaces the soul's name after sanitisation. Persists
// + rotates history. Returns the cleaned name on success.
func (a *Adjuster) SetName(name string) (string, error) {
	cleaned, err := SanitiseName(name)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.soul.Config.Name = cleaned
	if err := a.persistLocked(); err != nil {
		return "", err
	}
	return cleaned, nil
}

// AddFragment appends a sanitised fragment. Refuses duplicates and
// respects MaxFragments. Returns the cleaned fragment + the new
// total count on success.
func (a *Adjuster) AddFragment(text string) (string, int, error) {
	cleaned, err := SanitiseFragment(text)
	if err != nil {
		return "", 0, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, existing := range a.soul.Config.Fragments {
		if strings.EqualFold(existing, cleaned) {
			return "", 0, errors.New("fragment already present")
		}
	}
	if len(a.soul.Config.Fragments) >= MaxFragments {
		return "", 0, fmt.Errorf("fragment cap reached (%d) — remove one first", MaxFragments)
	}
	a.soul.Config.Fragments = append(a.soul.Config.Fragments, cleaned)
	if err := a.persistLocked(); err != nil {
		return "", 0, err
	}
	return cleaned, len(a.soul.Config.Fragments), nil
}

// RemoveFragment removes the first fragment whose lower-cased text
// contains needle (lower-cased). Substring match keeps the UX
// forgiving — "forget the liverpool thing" matches "User supports
// Liverpool FC". Returns the removed fragment.
func (a *Adjuster) RemoveFragment(needle string) (string, error) {
	needle = strings.ToLower(strings.TrimSpace(needle))
	if needle == "" {
		return "", errors.New("needle empty")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, f := range a.soul.Config.Fragments {
		if strings.Contains(strings.ToLower(f), needle) {
			removed := f
			a.soul.Config.Fragments = append(a.soul.Config.Fragments[:i], a.soul.Config.Fragments[i+1:]...)
			if err := a.persistLocked(); err != nil {
				return "", err
			}
			return removed, nil
		}
	}
	return "", errors.New("no fragment matched")
}

// ListFragments returns a copy of the current fragment list.
func (a *Adjuster) ListFragments() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, len(a.soul.Config.Fragments))
	copy(out, a.soul.Config.Fragments)
	return out
}

// Tune is the agent-callable adjuster for the bounded numeric
// emotive_style dimensions. delta is the desired change (+1 / -1
// typical); the result is clamped to 0-10 AND ±MaxDriftFromBaseline
// from the loaded baseline. Bypasses cooldown — Tune is intentional
// operator-or-owner action, not natural-language feedback drift.
func (a *Adjuster) Tune(dimension string, delta int) (int, int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	currentPtr, baselinePtr := a.dimensionPointers(dimension)
	if currentPtr == nil {
		return 0, 0, fmt.Errorf("unknown dimension %q (want excitement|formality|directness|sarcasm|humor)", dimension)
	}
	prev := *currentPtr
	newValue := prev + delta
	baseline := *baselinePtr
	if newValue > baseline+MaxDriftFromBaseline {
		newValue = baseline + MaxDriftFromBaseline
	}
	if newValue < baseline-MaxDriftFromBaseline {
		newValue = baseline - MaxDriftFromBaseline
	}
	if newValue < 0 {
		newValue = 0
	}
	if newValue > 10 {
		newValue = 10
	}
	if newValue == prev {
		return prev, newValue, fmt.Errorf("already at cap (current=%d, baseline=%d ± %d)", prev, baseline, MaxDriftFromBaseline)
	}
	*currentPtr = newValue
	if err := a.persistLocked(); err != nil {
		*currentPtr = prev
		return prev, prev, err
	}
	return prev, newValue, nil
}

// SetEmojiUsage sets the categorical emoji_usage field.
func (a *Adjuster) SetEmojiUsage(value string) error {
	switch value {
	case "minimal", "moderate", "generous":
	default:
		return fmt.Errorf("emoji_usage=%q must be minimal|moderate|generous", value)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.soul.Config.EmotiveStyle.EmojiUsage = value
	return a.persistLocked()
}

// HistoryRollback restores the soul to a previous version. steps=1
// reverts the most recent persist, steps=2 the one before, etc.
// Returns the timestamp string of the version restored.
func (a *Adjuster) HistoryRollback(steps int) (string, error) {
	if steps < 1 {
		return "", errors.New("steps must be >= 1")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.soul.Path == "" {
		return "", errors.New("no on-disk soul — nothing to rollback")
	}
	historyDir := filepath.Join(filepath.Dir(a.soul.Path), HistoryDirName)
	entries, err := os.ReadDir(historyDir)
	if err != nil {
		return "", fmt.Errorf("read history dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	if len(names) < steps {
		return "", fmt.Errorf("only %d history entries; cannot rollback %d steps", len(names), steps)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	pick := names[steps-1]

	raw, err := os.ReadFile(filepath.Join(historyDir, pick))
	if err != nil {
		return "", fmt.Errorf("read snapshot %q: %w", pick, err)
	}
	restored, err := Parse(raw, a.soul.Path)
	if err != nil {
		return "", fmt.Errorf("parse snapshot: %w", err)
	}
	a.soul.Config = restored.Config
	a.soul.Body = restored.Body
	if err := a.persistLocked(); err != nil {
		return "", err
	}
	return strings.TrimSuffix(pick, ".md"), nil
}

// snapshotHistoryLocked writes the current on-disk file (pre-write)
// to soul.d/history/<ts>.md and prunes anything past
// MaxHistoryVersions. Called from persistLocked before the new
// content overwrites the live file. Failures are non-fatal —
// history is a recovery convenience, not a write-path requirement.
func (a *Adjuster) snapshotHistoryLocked() {
	if a.soul.Path == "" {
		return
	}
	current, err := os.ReadFile(a.soul.Path)
	if err != nil {
		return
	}
	historyDir := filepath.Join(filepath.Dir(a.soul.Path), HistoryDirName)
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		return
	}
	ts := a.now().UTC().Format("20060102T150405.000")
	dst := filepath.Join(historyDir, ts+".md")
	if err := os.WriteFile(dst, current, 0o644); err != nil {
		return
	}
	a.pruneHistoryLocked(historyDir)
}

func (a *Adjuster) pruneHistoryLocked(historyDir string) {
	entries, err := os.ReadDir(historyDir)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	if len(names) <= MaxHistoryVersions {
		return
	}
	sort.Strings(names) // ascending → oldest first
	for _, old := range names[:len(names)-MaxHistoryVersions] {
		_ = os.Remove(filepath.Join(historyDir, old))
	}
}

// touch is exposed only for tests so they can advance the
// internal clock without exporting Now().
func (a *Adjuster) touch(t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastAdjusted["__touch"] = t
}
