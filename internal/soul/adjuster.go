package soul

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Adjuster owns the live Soul state. The agent's tunable subset
// (name, emotive dimensions, emoji_usage, fragments) is overlaid on
// the operator's immutable SOUL.md baseline at read time. Mutations
// write through a TuneStore — production wires a raft-backed store
// so cluster nodes converge; tests use MemoryTuneStore.
//
// SOUL.md on disk is read-only. The Adjuster never writes to it.
// This means container deployments don't need a writable file mount,
// configmap-as-symlink edge cases disappear, and the agent's
// personality stays consistent across nodes.
type Adjuster struct {
	mu         sync.RWMutex
	baseline   *Soul      // operator-curated, immutable for the lifetime of the process
	tune       *TuneState // cached overlay; refreshed on every Put
	store      TuneStore  // raft-backed in prod, in-memory in tests
	classifier Classifier
	now        func() time.Time

	// baselineEmotive is the EmotiveStyle as originally loaded.
	// MaxDriftFromBaseline is measured against THIS so drift can't
	// accumulate past ±3 over many adjustments.
	baselineEmotive types.EmotiveStyle

	// lastAdjusted per-dimension powers the cooldown for natural-
	// language feedback. Tune (the explicit operator/owner action)
	// bypasses cooldown intentionally.
	lastAdjusted map[string]time.Time
}

// MaxDriftFromBaseline is the ±3 cap any single emotive dimension
// can move from its baseline. Made a package-level constant so it's
// obvious + changeable at one site.
const MaxDriftFromBaseline = 3

// AdjusterConfig bundles the dependencies. Store is REQUIRED — pass
// MemoryTuneStore for tests that don't want raft. Classifier
// defaults to RegexClassifier when nil; now defaults to time.Now.
type AdjusterConfig struct {
	Soul       *Soul
	Store      TuneStore
	Classifier Classifier
	Now        func() time.Time
}

// NewAdjuster wires the Adjuster. Loads the current tune from the
// store at construction so Soul() reflects cluster state from boot
// (rather than transiently serving baseline-only until the first
// mutator runs).
func NewAdjuster(cfg AdjusterConfig) (*Adjuster, error) {
	if cfg.Soul == nil {
		return nil, errors.New("soul: Adjuster requires a Soul (baseline)")
	}
	if cfg.Store == nil {
		return nil, errors.New("soul: Adjuster requires a TuneStore")
	}
	classifier := cfg.Classifier
	if classifier == nil {
		classifier = NewRegexClassifier()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	a := &Adjuster{
		baseline:        cfg.Soul,
		store:           cfg.Store,
		classifier:      classifier,
		now:             now,
		baselineEmotive: cfg.Soul.Config.EmotiveStyle,
		lastAdjusted:    make(map[string]time.Time),
	}
	current, err := cfg.Store.Get(context.Background())
	if err != nil {
		return nil, fmt.Errorf("soul: load current tune: %w", err)
	}
	a.tune = current
	return a, nil
}

// Soul returns the merged baseline+tune view. The returned struct
// is a snapshot — callers must not mutate it.
func (a *Adjuster) Soul() Soul {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.mergedLocked()
}

// mergedLocked builds the live Soul by overlaying tune on baseline.
// Caller holds a.mu (read or write).
func (a *Adjuster) mergedLocked() Soul {
	out := *a.baseline
	out.Config = a.baseline.Config
	out.Config.Fragments = append([]string(nil), a.baseline.Config.Fragments...)
	if a.tune == nil {
		return out
	}
	if a.tune.Name != nil {
		out.Config.Name = *a.tune.Name
	}
	if a.tune.Excitement != nil {
		out.Config.EmotiveStyle.Excitement = *a.tune.Excitement
	}
	if a.tune.Formality != nil {
		out.Config.EmotiveStyle.Formality = *a.tune.Formality
	}
	if a.tune.Directness != nil {
		out.Config.EmotiveStyle.Directness = *a.tune.Directness
	}
	if a.tune.Sarcasm != nil {
		out.Config.EmotiveStyle.Sarcasm = *a.tune.Sarcasm
	}
	if a.tune.Humor != nil {
		out.Config.EmotiveStyle.Humor = *a.tune.Humor
	}
	if a.tune.EmojiUsage != nil {
		out.Config.EmotiveStyle.EmojiUsage = *a.tune.EmojiUsage
	}
	if a.tune.Fragments != nil {
		out.Config.Fragments = append([]string(nil), (*a.tune.Fragments)...)
	}
	return out
}

// emotiveValueLocked returns the current effective value for an
// emotive dimension — tune override if present, baseline otherwise.
// Caller holds a.mu.
func (a *Adjuster) emotiveValueLocked(name string) (current, baseline int, ok bool) {
	switch name {
	case "excitement":
		baseline = a.baselineEmotive.Excitement
		current = baseline
		if a.tune != nil && a.tune.Excitement != nil {
			current = *a.tune.Excitement
		}
		return current, baseline, true
	case "formality":
		baseline = a.baselineEmotive.Formality
		current = baseline
		if a.tune != nil && a.tune.Formality != nil {
			current = *a.tune.Formality
		}
		return current, baseline, true
	case "directness":
		baseline = a.baselineEmotive.Directness
		current = baseline
		if a.tune != nil && a.tune.Directness != nil {
			current = *a.tune.Directness
		}
		return current, baseline, true
	case "sarcasm":
		baseline = a.baselineEmotive.Sarcasm
		current = baseline
		if a.tune != nil && a.tune.Sarcasm != nil {
			current = *a.tune.Sarcasm
		}
		return current, baseline, true
	case "humor":
		baseline = a.baselineEmotive.Humor
		current = baseline
		if a.tune != nil && a.tune.Humor != nil {
			current = *a.tune.Humor
		}
		return current, baseline, true
	}
	return 0, 0, false
}

// setEmotiveLocked writes a new value to the named field on a tune
// state copy. Returns an error for unknown dimensions.
func setEmotiveLocked(state *TuneState, name string, value int) error {
	v := value
	switch name {
	case "excitement":
		state.Excitement = &v
	case "formality":
		state.Formality = &v
	case "directness":
		state.Directness = &v
	case "sarcasm":
		state.Sarcasm = &v
	case "humor":
		state.Humor = &v
	default:
		return fmt.Errorf("unknown dimension %q", name)
	}
	return nil
}

// ApplyResult describes the outcome of an Apply call. See the
// per-field semantics in the existing comment.
type ApplyResult struct {
	Applied   bool
	Dimension string
	Direction Direction
	PrevValue int
	NewValue  int
	Reason    string
}

// Apply processes one feedback utterance: classify → cooldown check →
// clamp to ±3 from baseline → persist new tune state. The cooldown
// state is in-memory per process so it doesn't replicate; that's
// intentional — natural-language feedback should be node-local
// because two nodes won't see the same utterance.
func (a *Adjuster) Apply(ctx context.Context, utterance string) (*ApplyResult, error) {
	fb, err := a.classifier.Classify(ctx, utterance)
	if err != nil {
		return &ApplyResult{Applied: false, Reason: "could not classify feedback"}, nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if last, ok := a.lastAdjusted[fb.Dimension]; ok {
		elapsed := a.now().Sub(last)
		cooldown := a.baseline.Config.Adjustments.CooldownPeriod
		if cooldown > 0 && elapsed < cooldown {
			return &ApplyResult{
				Applied:   false,
				Dimension: fb.Dimension,
				Direction: fb.Direction,
				Reason:    fmt.Sprintf("%s adjustment cooling down (%v remaining)", fb.Dimension, cooldown-elapsed),
			}, nil
		}
	}

	prev, baseline, ok := a.emotiveValueLocked(fb.Dimension)
	if !ok {
		return &ApplyResult{Applied: false, Dimension: fb.Dimension, Reason: "dimension not addressable — internal error"}, nil
	}

	coefficient := a.baseline.Config.Adjustments.FeedbackCoefficient
	if coefficient <= 0 {
		coefficient = 0.15
	}
	delta := int(math.Round(coefficient * 10 * float64(fb.Direction)))
	if delta == 0 {
		return &ApplyResult{
			Applied:   false,
			Dimension: fb.Dimension,
			Direction: fb.Direction,
			PrevValue: prev,
			Reason:    "adjustment below rounding threshold — no change",
		}, nil
	}

	newValue := clamp(prev+delta, baseline)
	if newValue == prev {
		return &ApplyResult{
			Applied:   false,
			Dimension: fb.Dimension,
			Direction: fb.Direction,
			PrevValue: prev,
			NewValue:  newValue,
			Reason:    "already at cap from baseline",
		}, nil
	}

	state := a.tune.Clone()
	if err := setEmotiveLocked(state, fb.Dimension, newValue); err != nil {
		return nil, err
	}
	state.UpdatedBy = "feedback"
	if err := a.store.Put(ctx, state); err != nil {
		return nil, fmt.Errorf("soul: persist: %w", err)
	}
	a.tune = state
	a.lastAdjusted[fb.Dimension] = a.now()

	return &ApplyResult{
		Applied:   true,
		Dimension: fb.Dimension,
		Direction: fb.Direction,
		PrevValue: prev,
		NewValue:  newValue,
		Reason:    fb.Reason,
	}, nil
}

// clamp enforces the 0..10 hard bounds AND ±MaxDriftFromBaseline
// from baseline. Centralised so Apply + Tune use identical logic.
func clamp(value, baseline int) int {
	if value > baseline+MaxDriftFromBaseline {
		value = baseline + MaxDriftFromBaseline
	}
	if value < baseline-MaxDriftFromBaseline {
		value = baseline - MaxDriftFromBaseline
	}
	if value < 0 {
		value = 0
	}
	if value > 10 {
		value = 10
	}
	return value
}

// CooldownRemaining reports how long before the named dimension can
// be adjusted again via natural-language feedback. Tune (explicit
// owner action) is not gated by cooldown.
func (a *Adjuster) CooldownRemaining(dimension string) time.Duration {
	a.mu.RLock()
	defer a.mu.RUnlock()
	last, ok := a.lastAdjusted[dimension]
	if !ok {
		return 0
	}
	cooldown := a.baseline.Config.Adjustments.CooldownPeriod
	if cooldown <= 0 {
		return 0
	}
	elapsed := a.now().Sub(last)
	if elapsed >= cooldown {
		return 0
	}
	return cooldown - elapsed
}

// touch is exposed only for tests so they can advance the internal
// clock without exporting Now().
func (a *Adjuster) touch(t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastAdjusted["__touch"] = t
}
