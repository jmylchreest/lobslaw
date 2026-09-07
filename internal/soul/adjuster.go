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
	baseline   *Soul      // operator-curated, replaced under mu on operator reload
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
	// DeferLoad permits remote stores to wait for discovery before their first read.
	DeferLoad bool
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
	if cfg.DeferLoad {
		return a, nil
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
		out.Config.EmotiveStyle.Excitement = clamp(*a.tune.Excitement, a.baselineEmotive.Excitement)
	}
	if a.tune.Formality != nil {
		out.Config.EmotiveStyle.Formality = clamp(*a.tune.Formality, a.baselineEmotive.Formality)
	}
	if a.tune.Directness != nil {
		out.Config.EmotiveStyle.Directness = clamp(*a.tune.Directness, a.baselineEmotive.Directness)
	}
	if a.tune.Sarcasm != nil {
		out.Config.EmotiveStyle.Sarcasm = clamp(*a.tune.Sarcasm, a.baselineEmotive.Sarcasm)
	}
	if a.tune.Humor != nil {
		out.Config.EmotiveStyle.Humor = clamp(*a.tune.Humor, a.baselineEmotive.Humor)
	}
	if a.tune.EmojiUsage != nil {
		out.Config.EmotiveStyle.EmojiUsage = *a.tune.EmojiUsage
	}
	if a.tune.Fragments != nil {
		out.Config.Fragments = append([]string(nil), (*a.tune.Fragments)...)
	}
	out.Overrides = a.tune.Fields()
	return out
}

// emotiveValueLocked returns the current effective value for an
// emotive dimension — tune override if present, baseline otherwise.
// Caller holds a.mu.
func (a *Adjuster) emotiveValueLocked(name string) (current, baseline int, ok bool) {
	switch name {
	case DimExcitement:
		baseline = a.baselineEmotive.Excitement
		current = baseline
		if a.tune != nil && a.tune.Excitement != nil {
			current = clamp(*a.tune.Excitement, a.baselineEmotive.Excitement)
		}
		return current, baseline, true
	case DimFormality:
		baseline = a.baselineEmotive.Formality
		current = baseline
		if a.tune != nil && a.tune.Formality != nil {
			current = clamp(*a.tune.Formality, a.baselineEmotive.Formality)
		}
		return current, baseline, true
	case DimDirectness:
		baseline = a.baselineEmotive.Directness
		current = baseline
		if a.tune != nil && a.tune.Directness != nil {
			current = clamp(*a.tune.Directness, a.baselineEmotive.Directness)
		}
		return current, baseline, true
	case DimSarcasm:
		baseline = a.baselineEmotive.Sarcasm
		current = baseline
		if a.tune != nil && a.tune.Sarcasm != nil {
			current = clamp(*a.tune.Sarcasm, a.baselineEmotive.Sarcasm)
		}
		return current, baseline, true
	case DimHumor:
		baseline = a.baselineEmotive.Humor
		current = baseline
		if a.tune != nil && a.tune.Humor != nil {
			current = clamp(*a.tune.Humor, a.baselineEmotive.Humor)
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
	case DimExcitement:
		state.Excitement = &v
	case DimFormality:
		state.Formality = &v
	case DimDirectness:
		state.Directness = &v
	case DimSarcasm:
		state.Sarcasm = &v
	case DimHumor:
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
	if err := a.refreshLocked(ctx); err != nil {
		return nil, err
	}

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

// ReplaceBaseline publishes an operator edit without discarding explicit
// tuning. Bounds are applied at read time so replicas never rewrite the
// cluster overlay merely because their local baseline changed.
func (a *Adjuster) ReplaceBaseline(baseline *Soul) {
	if baseline == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.baseline = baseline
	a.baselineEmotive = baseline.Config.EmotiveStyle
	a.lastAdjusted = make(map[string]time.Time)
}

// Snapshot refreshes and merges under one lock. Remote stores are read at
// turn/tool boundaries, so a compute-only node cannot serve a boot snapshot
// forever or silently use an empty overlay when its cluster is unavailable.
func (a *Adjuster) Snapshot(ctx context.Context) (Soul, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.refreshLocked(ctx); err != nil {
		return Soul{}, err
	}
	return a.mergedLocked(), nil
}

func (a *Adjuster) refreshLocked(ctx context.Context) error {
	current, err := a.store.Get(ctx)
	if err != nil {
		return err
	}
	// A follower may briefly serve an older revision after a forwarded write.
	if a.tune == nil || current != nil && current.Revision >= a.tune.Revision {
		a.tune = current
	}
	return nil
}
