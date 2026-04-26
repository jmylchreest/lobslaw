package soul

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Adjuster owns the live Soul state and mutates emotive dimensions
// in response to user feedback. One instance per node — the soul is
// node-local state (not Raft-replicated) because personalization is
// inherently a local conversation, and an operator running N nodes
// with the same SOUL.md stays consistent through the shared file
// rather than through Raft.
type Adjuster struct {
	mu         sync.RWMutex
	soul       *Soul
	classifier Classifier
	now        func() time.Time // clock injection for tests

	// baseline is the EmotiveStyle as originally loaded. The ±3 cap
	// is measured against THIS, not the current (mutated) state, so
	// drift can't accumulate past the bound over many adjustments.
	baseline types.EmotiveStyle

	// lastAdjusted per-dimension so we can enforce the cooldown. Map
	// key is the dimension name; absent = never adjusted.
	lastAdjusted map[string]time.Time
}

// MaxDriftFromBaseline is the ±3 cap any single dimension can move
// from its baseline value. Matches the PLAN.md requirement. Made a
// package-level constant so it's obvious + changeable at one site.
const MaxDriftFromBaseline = 3

// AdjusterConfig bundles the dependencies. Classifier defaults to
// RegexClassifier when nil; now defaults to time.Now.
type AdjusterConfig struct {
	Soul       *Soul
	Classifier Classifier
	Now        func() time.Time
}

// NewAdjuster wires the adjuster. A nil Soul is a boot error
// (callers should route through LoadOrDefault first); nil
// Classifier falls back to the offline regex path.
func NewAdjuster(cfg AdjusterConfig) (*Adjuster, error) {
	if cfg.Soul == nil {
		return nil, errors.New("soul: Adjuster requires a Soul")
	}
	classifier := cfg.Classifier
	if classifier == nil {
		classifier = NewRegexClassifier()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	baseline := cfg.Soul.Config.EmotiveStyle
	return &Adjuster{
		soul:         cfg.Soul,
		classifier:   classifier,
		now:          now,
		baseline:     baseline,
		lastAdjusted: make(map[string]time.Time),
	}, nil
}

// Soul returns a snapshot of the current Soul. Safe for concurrent
// reads; callers should not mutate the returned struct.
func (a *Adjuster) Soul() Soul {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return *a.soul
}

// ApplyResult describes the outcome of an Apply call. Applied=true
// means the dimension moved AND the soul was persisted. When
// Applied=false, Reason explains why — cooldown active, cap
// reached, unclassifiable feedback, etc. The channel handler
// surfaces Reason to the user so "I heard you but didn't change"
// is distinguishable from "I didn't understand you."
type ApplyResult struct {
	Applied    bool
	Dimension  string
	Direction  Direction
	PrevValue  int
	NewValue   int
	Reason     string
}

// Apply processes one feedback utterance end-to-end: classify →
// cooldown check → clamp to ±3 from baseline → write new value →
// persist to SOUL.md. Returns ApplyResult describing the outcome
// (including the reason when no change happened).
func (a *Adjuster) Apply(ctx context.Context, utterance string) (*ApplyResult, error) {
	fb, err := a.classifier.Classify(ctx, utterance)
	if err != nil {
		return &ApplyResult{
			Applied: false,
			Reason:  "could not classify feedback",
		}, nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if last, ok := a.lastAdjusted[fb.Dimension]; ok {
		elapsed := a.now().Sub(last)
		cooldown := a.soul.Config.Adjustments.CooldownPeriod
		if cooldown > 0 && elapsed < cooldown {
			return &ApplyResult{
				Applied:   false,
				Dimension: fb.Dimension,
				Direction: fb.Direction,
				Reason: fmt.Sprintf("%s adjustment cooling down (%v remaining)",
					fb.Dimension, cooldown-elapsed),
			}, nil
		}
	}

	currentPtr, baselinePtr := a.dimensionPointers(fb.Dimension)
	if currentPtr == nil {
		return &ApplyResult{
			Applied:   false,
			Dimension: fb.Dimension,
			Reason:    "dimension not addressable — internal error",
		}, nil
	}
	prev := *currentPtr

	// Direction's sign drives the direction of the change:
	//   decrease (−1) × coef × 10 → negative delta → lower value
	//   increase (+1) × coef × 10 → positive delta → higher value
	// Coefficient is a 0–1 fraction of the 0–10 scale, rounded to
	// an int for a crisp user-facing value.
	coefficient := a.soul.Config.Adjustments.FeedbackCoefficient
	if coefficient <= 0 {
		coefficient = 0.15
	}
	delta := int(math.Round(coefficient * 10 * float64(fb.Direction)))
	if delta == 0 {
		// Coefficient × 10 < 0.5 rounds to zero — treat as "too
		// small to matter this turn" rather than a silent no-op.
		return &ApplyResult{
			Applied:   false,
			Dimension: fb.Dimension,
			Direction: fb.Direction,
			PrevValue: prev,
			Reason:    "adjustment below rounding threshold — no change",
		}, nil
	}

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
		return &ApplyResult{
			Applied:   false,
			Dimension: fb.Dimension,
			Direction: fb.Direction,
			PrevValue: prev,
			NewValue:  newValue,
			Reason:    "already at cap from baseline",
		}, nil
	}

	*currentPtr = newValue
	a.lastAdjusted[fb.Dimension] = a.now()

	if err := a.persistLocked(); err != nil {
		// Roll back the in-memory change — otherwise the agent's
		// Soul view and the on-disk file would diverge.
		*currentPtr = prev
		return nil, fmt.Errorf("soul: persist: %w", err)
	}

	return &ApplyResult{
		Applied:   true,
		Dimension: fb.Dimension,
		Direction: fb.Direction,
		PrevValue: prev,
		NewValue:  newValue,
		Reason:    fb.Reason,
	}, nil
}

// dimensionPointers returns in-place pointers to the live
// EmotiveStyle field + its baseline counterpart so Apply can mutate
// + clamp in one place. Returns (nil, nil) for unknown names.
func (a *Adjuster) dimensionPointers(name string) (*int, *int) {
	style := &a.soul.Config.EmotiveStyle
	base := &a.baseline
	switch name {
	case "excitement":
		return &style.Excitement, &base.Excitement
	case "formality":
		return &style.Formality, &base.Formality
	case "directness":
		return &style.Directness, &base.Directness
	case "sarcasm":
		return &style.Sarcasm, &base.Sarcasm
	case "humor":
		return &style.Humor, &base.Humor
	}
	return nil, nil
}

// persistLocked writes the current Soul back to disk. Preserves the
// markdown body verbatim and re-serialises only the YAML frontmatter
// so operator comments / blank lines in the body don't get lost on
// every adjustment. Caller must hold a.mu.
//
// Before overwriting, snapshots the current on-disk file to
// soul.d/history/<ts>.md so HistoryRollback can recover a known-good
// version after a regrettable agent self-edit.
func (a *Adjuster) persistLocked() error {
	if a.soul.Path == "" {
		// No on-disk representation (e.g. DefaultSoul). In-memory
		// mutation only. Safe outcome: nothing to persist.
		return nil
	}
	a.snapshotHistoryLocked()
	encoded, err := encodeFrontmatter(a.soul.Config)
	if err != nil {
		return err
	}
	// Trailing newline on the body keeps the file POSIX-compliant
	// (last line ends in newline) and means diffs only show the
	// frontmatter changes, not a shifted EOF marker.
	out := "---\n" + string(encoded) + "---\n\n" + a.soul.Body
	if len(a.soul.Body) > 0 && a.soul.Body[len(a.soul.Body)-1] != '\n' {
		out += "\n"
	}
	return os.WriteFile(a.soul.Path, []byte(out), 0o644)
}

// encodeFrontmatter serialises SoulConfig as YAML with deterministic
// key ordering so diffs between adjustments are minimal (one line
// changed rather than a full rewrite). yaml.Marshal uses the struct
// field order which is stable across runs.
func encodeFrontmatter(cfg types.SoulConfig) ([]byte, error) {
	var buf yamlBuffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&cfg); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// yamlBuffer is a minimal bytes.Buffer shim that satisfies
// io.Writer. Wrapping the standard library type avoids pulling
// "bytes" into this file's imports just for one stanza.
type yamlBuffer struct{ buf []byte }

func (b *yamlBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *yamlBuffer) Bytes() []byte { return b.buf }

// CooldownRemaining reports how long before the given dimension
// can be adjusted again. Returns 0 when the dimension has never
// been adjusted OR its cooldown has elapsed. Exposed for UI / CLI
// surfaces that want to show "you already adjusted sarcasm — next
// available in 4h."
func (a *Adjuster) CooldownRemaining(dimension string) time.Duration {
	a.mu.RLock()
	defer a.mu.RUnlock()
	last, ok := a.lastAdjusted[dimension]
	if !ok {
		return 0
	}
	cooldown := a.soul.Config.Adjustments.CooldownPeriod
	if cooldown <= 0 {
		return 0
	}
	elapsed := a.now().Sub(last)
	if elapsed >= cooldown {
		return 0
	}
	return cooldown - elapsed
}
