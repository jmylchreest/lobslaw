package soul

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SetName replaces the soul's name after sanitisation. Persists via
// the TuneStore (raft in prod). Returns the cleaned name on success.
func (a *Adjuster) SetName(ctx context.Context, name string) (string, error) {
	cleaned, err := SanitiseName(name)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.tune.Clone()
	state.Name = &cleaned
	state.UpdatedBy = "soul_tune"
	if err := a.store.Put(ctx, state); err != nil {
		return "", err
	}
	a.tune = state
	return cleaned, nil
}

// AddFragment appends a sanitised fragment. Refuses duplicates and
// respects MaxFragments. Returns the cleaned fragment + the new
// total count on success.
func (a *Adjuster) AddFragment(ctx context.Context, text string) (string, int, error) {
	cleaned, err := SanitiseFragment(text)
	if err != nil {
		return "", 0, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.currentFragmentsLocked()
	for _, existing := range current {
		if strings.EqualFold(existing, cleaned) {
			return "", 0, errors.New("fragment already present")
		}
	}
	if len(current) >= MaxFragments {
		return "", 0, fmt.Errorf("fragment cap reached (%d) — remove one first", MaxFragments)
	}
	updated := append(append([]string(nil), current...), cleaned)
	state := a.tune.Clone()
	state.Fragments = &updated
	state.UpdatedBy = "soul_tune"
	if err := a.store.Put(ctx, state); err != nil {
		return "", 0, err
	}
	a.tune = state
	return cleaned, len(updated), nil
}

// RemoveFragment removes the first fragment whose lower-cased text
// contains needle (lower-cased). Substring match keeps the UX
// forgiving — "forget the liverpool thing" matches "User supports
// Liverpool FC". Returns the removed fragment.
func (a *Adjuster) RemoveFragment(ctx context.Context, needle string) (string, error) {
	needle = strings.ToLower(strings.TrimSpace(needle))
	if needle == "" {
		return "", errors.New("needle empty")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.currentFragmentsLocked()
	for i, f := range current {
		if strings.Contains(strings.ToLower(f), needle) {
			removed := f
			updated := append(append([]string(nil), current[:i]...), current[i+1:]...)
			state := a.tune.Clone()
			state.Fragments = &updated
			state.UpdatedBy = "soul_tune"
			if err := a.store.Put(ctx, state); err != nil {
				return "", err
			}
			a.tune = state
			return removed, nil
		}
	}
	return "", errors.New("no fragment matched")
}

// ListFragments returns a copy of the merged fragment list.
func (a *Adjuster) ListFragments() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	current := a.currentFragmentsLocked()
	out := make([]string, len(current))
	copy(out, current)
	return out
}

// currentFragmentsLocked returns the merged fragment list (tune
// override or baseline). Caller holds a.mu.
func (a *Adjuster) currentFragmentsLocked() []string {
	if a.tune != nil && a.tune.Fragments != nil {
		return *a.tune.Fragments
	}
	return a.baseline.Config.Fragments
}

// Tune is the agent-callable adjuster for the bounded numeric
// emotive_style dimensions. delta is the desired change (+1 / -1
// typical); the result is clamped to 0–10 AND ±MaxDriftFromBaseline
// from the loaded baseline. Bypasses cooldown — Tune is intentional
// operator-or-owner action, not natural-language feedback drift.
func (a *Adjuster) Tune(ctx context.Context, dimension string, delta int) (int, int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	prev, baseline, ok := a.emotiveValueLocked(dimension)
	if !ok {
		return 0, 0, fmt.Errorf("unknown dimension %q (want excitement|formality|directness|sarcasm|humor)", dimension)
	}
	newValue := clamp(prev+delta, baseline)
	if newValue == prev {
		return prev, newValue, fmt.Errorf("already at cap (current=%d, baseline=%d ± %d)", prev, baseline, MaxDriftFromBaseline)
	}
	state := a.tune.Clone()
	if err := setEmotiveLocked(state, dimension, newValue); err != nil {
		return prev, prev, err
	}
	state.UpdatedBy = "soul_tune"
	if err := a.store.Put(ctx, state); err != nil {
		return prev, prev, err
	}
	a.tune = state
	return prev, newValue, nil
}

// SetEmojiUsage sets the categorical emoji_usage field.
func (a *Adjuster) SetEmojiUsage(ctx context.Context, value string) error {
	switch value {
	case "minimal", "moderate", "generous":
	default:
		return fmt.Errorf("emoji_usage=%q must be minimal|moderate|generous", value)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.tune.Clone()
	state.EmojiUsage = &value
	state.UpdatedBy = "soul_tune"
	if err := a.store.Put(ctx, state); err != nil {
		return err
	}
	a.tune = state
	return nil
}

// HistoryRollback restores the soul tune to a previous version.
// steps=1 reverts the most recent change. Returns a human-readable
// label describing the restored state.
func (a *Adjuster) HistoryRollback(ctx context.Context, steps int) (string, error) {
	if steps < 1 {
		return "", errors.New("steps must be >= 1")
	}
	restored, err := a.store.Rollback(ctx, steps)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.tune = restored
	a.mu.Unlock()
	if restored != nil && !restored.UpdatedAt.IsZero() {
		return restored.UpdatedAt.UTC().Format("20060102T150405.000"), nil
	}
	return fmt.Sprintf("rolled back %d steps", steps), nil
}

// RefreshTune re-reads the current tune from the store. Wired by
// the node package as the FSM soul-tune change hook so a remote
// mutation (other node became leader) propagates to this Adjuster's
// in-memory cache without a process restart.
func (a *Adjuster) RefreshTune(ctx context.Context) error {
	current, err := a.store.Get(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.tune = current
	a.mu.Unlock()
	return nil
}
