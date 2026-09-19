package compute

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

type budgetCtxKey struct{}

// WithBudget attaches a turn budget so a child (ask_bot) can draw on it.
func WithBudget(ctx context.Context, b *TurnBudget) context.Context {
	if b == nil {
		return ctx
	}
	return context.WithValue(ctx, budgetCtxKey{}, b)
}

// BudgetFrom returns the reservation attached to ctx, or nil.
func BudgetFrom(ctx context.Context) *TurnBudget {
	b, _ := ctx.Value(budgetCtxKey{}).(*TurnBudget)
	return b
}

// TurnBudget tracks per-turn resource consumption against operator-
// configured caps. The agent loop (Phase 5.4) holds one TurnBudget
// per in-flight turn and increments it on every LLM call, tool
// invocation, or egress byte count.
//
// Safe for concurrent use — tool invocations within a turn may
// run in parallel even though the agent loop proper is sequential;
// the mutex keeps counters consistent regardless.
//
// On exceed, caller branches on the returned BudgetDecision. The
// agent loop converts Exceeded into a require_confirmation response
// so the user can approve continuing or terminate the turn.
type TurnBudget struct {
	parent *TurnBudget
	// Caps are the operator-configured limits. Copied in at
	// construction so a mid-turn config reload doesn't change
	// budget semantics for in-flight turns.
	caps BudgetCaps

	mu          sync.Mutex
	toolCalls   int
	spendUSD    float64
	egressBytes int64
	records     []CostRecord
}

// BudgetCaps is the operator-configured per-turn limit set.
type BudgetCaps = turn.BudgetCaps

// FromConfig builds BudgetCaps from the deprecated [compute.budgets]
// block. Retained for back-compat; new config should use
// FromLimits against [compute.limits] (which only carries the
// tool-call safety valve — no spend/egress rationing).
func FromConfig(cfg config.BudgetsConfig) BudgetCaps {
	return BudgetCaps{
		MaxToolCalls:   cfg.MaxToolCallsPerTurn,
		MaxSpendUSD:    cfg.MaxSpendUSDPerTurn,
		MaxEgressBytes: cfg.MaxEgressBytesPerTurn,
	}
}

// FromLimits builds BudgetCaps from the current [compute.limits]
// block. Spend + egress always zero (disabled); only the tool-call
// safety valve is carried through. Default 30 when unset.
func FromLimits(cfg config.LimitsConfig) BudgetCaps {
	cap := cfg.MaxToolCallsPerTurn
	if cap == 0 {
		cap = DefaultMaxToolCallsPerTurn
	}
	return BudgetCaps{MaxToolCalls: cap}
}

// FromComputeConfig picks Limits when non-zero, falling back to
// Budgets.MaxToolCallsPerTurn for back-compat on unmigrated configs.
// One-call API for wiring sites that don't want to know which
// section an operator chose.
func FromComputeConfig(cfg config.ComputeConfig) BudgetCaps {
	if cfg.Limits.MaxToolCallsPerTurn > 0 {
		return FromLimits(cfg.Limits)
	}
	return FromConfig(cfg.Budgets)
}

// BudgetDecision is the result of a Check / Record call. Exceeded
// means a cap has been passed; the caller surfaces require_confirmation.
// Within means the operation fits (or no cap is set on that dimension).
type BudgetDecision struct {
	Within     bool
	Exceeded   bool
	ExceededOn string // "tool_calls" | "spend" | "egress"; empty when Within
	Current    BudgetState
}

// BudgetState is a snapshot of consumed resources.
type BudgetState = turn.BudgetState

// ErrBudgetConfigInvalid fires when NewTurnBudget receives caps
// with negative values. Positive caps are enforced; zero means
// unlimited; negative is always a config error.
var ErrBudgetConfigInvalid = errors.New("turn budget: negative cap is invalid")

// NewTurnBudget constructs a budget. Returns an error for negative
// cap values (positive = limit, 0 = unlimited). Deliberately strict:
// "what if the user set -1 to mean unlimited" is exactly the kind
// of ambiguity that hides a bug.
func NewTurnBudget(caps BudgetCaps) (*TurnBudget, error) {
	if caps.MaxToolCalls < 0 || caps.MaxSpendUSD < 0 || caps.MaxEgressBytes < 0 {
		return nil, fmt.Errorf("%w: %+v", ErrBudgetConfigInvalid, caps)
	}
	return &TurnBudget{caps: caps}, nil
}

// Child has its own allowance, while all spending also consumes the parent's
// remaining budget. Tightening a specialist must never alter its caller's caps.
func (b *TurnBudget) Child(caps BudgetCaps) (*TurnBudget, error) {
	child, err := NewTurnBudget(caps)
	if err != nil {
		return nil, err
	}
	child.parent = b
	return child, nil
}

// RecordToolCall increments the tool-call counter and returns a
// decision. Called by the agent loop BEFORE dispatching a tool
// invocation; if Exceeded, the loop returns require_confirmation
// without invoking the tool.
func (b *TurnBudget) RecordToolCall() BudgetDecision {
	if b.parent != nil {
		if d := b.parent.RecordToolCall(); d.Exceeded {
			return d
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.toolCalls++
	if b.caps.MaxToolCalls > 0 && b.toolCalls > b.caps.MaxToolCalls {
		return b.exceededLocked("tool_calls")
	}
	return b.withinLocked()
}

// RecordCostUSD adds to the spend counter. Returns Exceeded when
// the running total has passed the cap. Also appends the CostRecord
// to the audit list so the caller can retrieve the full trail at
// turn end.
func (b *TurnBudget) RecordCostUSD(rec CostRecord) BudgetDecision {
	var parentDecision BudgetDecision
	if b.parent != nil {
		parentDecision = b.parent.RecordCostUSD(rec)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spendUSD += rec.CostUSD
	b.records = append(b.records, rec)
	if parentDecision.Exceeded {
		return parentDecision
	}
	if b.caps.MaxSpendUSD > 0 && b.spendUSD > b.caps.MaxSpendUSD {
		return b.exceededLocked("spend")
	}
	return b.withinLocked()
}

// RecordEgressBytes adds to the egress byte counter. Agent loop
// calls this after a tool invocation whose output went off-host
// (network tool output, file uploads). For purely-local tools it's
// a no-op; callers pass 0 when not applicable.
func (b *TurnBudget) RecordEgressBytes(n int64) BudgetDecision {
	var parentDecision BudgetDecision
	if b.parent != nil {
		parentDecision = b.parent.RecordEgressBytes(n)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.egressBytes += n
	if parentDecision.Exceeded {
		return parentDecision
	}
	if b.caps.MaxEgressBytes > 0 && b.egressBytes > b.caps.MaxEgressBytes {
		return b.exceededLocked("egress")
	}
	return b.withinLocked()
}

// Check returns the current decision without incrementing anything.
// Agent loop uses this to peek at state mid-turn for the user-
// facing "you've spent $0.42 of your $1.00 budget" display.
func (b *TurnBudget) Check() BudgetDecision {
	if b.parent != nil {
		if d := b.parent.Check(); d.Exceeded {
			return d
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.caps.MaxToolCalls > 0 && b.toolCalls > b.caps.MaxToolCalls {
		return b.exceededLocked("tool_calls")
	}
	if b.caps.MaxSpendUSD > 0 && b.spendUSD > b.caps.MaxSpendUSD {
		return b.exceededLocked("spend")
	}
	if b.caps.MaxEgressBytes > 0 && b.egressBytes > b.caps.MaxEgressBytes {
		return b.exceededLocked("egress")
	}
	return b.withinLocked()
}

// State returns a snapshot of current counters. Read-only; safe to
// call from any goroutine.
func (b *TurnBudget) State() BudgetState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stateLocked()
}

// Records returns the accumulated CostRecords. Caller receives a
// defensive copy so subsequent writes don't mutate it.
func (b *TurnBudget) Records() []CostRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]CostRecord, len(b.records))
	copy(out, b.records)
	return out
}

// Caps returns the operator-configured caps. Zero fields mean
// "unlimited on that dimension".
func (b *TurnBudget) Caps() BudgetCaps {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.caps
}

// Tighten narrows this budget's caps, never widens them.
func (b *TurnBudget) Tighten(caps BudgetCaps) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.caps = mergeCaps(b.caps, caps)
}

func mergeCaps(base, extra BudgetCaps) BudgetCaps {
	out := base
	if extra.MaxToolCalls > 0 && (out.MaxToolCalls == 0 || extra.MaxToolCalls < out.MaxToolCalls) {
		out.MaxToolCalls = extra.MaxToolCalls
	}
	if extra.MaxSpendUSD > 0 && (out.MaxSpendUSD == 0 || extra.MaxSpendUSD < out.MaxSpendUSD) {
		out.MaxSpendUSD = extra.MaxSpendUSD
	}
	if extra.MaxEgressBytes > 0 && (out.MaxEgressBytes == 0 || extra.MaxEgressBytes < out.MaxEgressBytes) {
		out.MaxEgressBytes = extra.MaxEgressBytes
	}
	return out
}

// Restore replays already-spent budget onto a fresh TurnBudget, for a
// turn resuming after a confirmation — possibly on a different node
// and after a restart, so there is no live budget to carry over.
//
// Counters only move FORWARD. Without that, a confirmation would be a
// way to reset the allowance: pause a turn near its cap, resume it
// with a smaller restored figure, and spend the difference again. The
// caps themselves are deliberately not restored — they come from the
// resuming node's current config, so an operator lowering a limit is
// not overridden by a turn that started before the change.
func (b *TurnBudget) Restore(state BudgetState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if state.ToolCalls > b.toolCalls {
		b.toolCalls = state.ToolCalls
	}
	if state.SpendUSD > b.spendUSD {
		b.spendUSD = state.SpendUSD
	}
	if state.EgressBytes > b.egressBytes {
		b.egressBytes = state.EgressBytes
	}
}

// Relax lifts every cap for the rest of this turn — all three
// dimensions go to "unlimited". Used by channel handlers after a
// user explicitly approves continuing past a budget-triggered
// confirmation prompt. Semantics: approval applies to the REMAINDER
// of this turn only; subsequent turns construct a fresh TurnBudget
// from config.
//
// Existing counters are preserved (and visible via State) so audit
// still reflects what was spent before + after the approval. Only
// the caps change.
func (b *TurnBudget) Relax() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.caps = BudgetCaps{}
}

// ---- lock-held helpers ----

func (b *TurnBudget) withinLocked() BudgetDecision {
	return BudgetDecision{
		Within:  true,
		Current: b.stateLocked(),
	}
}

func (b *TurnBudget) exceededLocked(dim string) BudgetDecision {
	return BudgetDecision{
		Exceeded:   true,
		ExceededOn: dim,
		Current:    b.stateLocked(),
	}
}

func (b *TurnBudget) stateLocked() BudgetState {
	return BudgetState{
		ToolCalls:   b.toolCalls,
		SpendUSD:    b.spendUSD,
		EgressBytes: b.egressBytes,
	}
}
