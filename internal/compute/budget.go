package compute

import (
	"errors"
	"fmt"
	"sync"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

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

// BudgetCaps mirrors the config.BudgetsConfig shape but with
// clearer types for in-code use. Zero on any field means "no cap"
// for that dimension — operators commonly set only spend and leave
// tool-calls / egress unbounded.
type BudgetCaps struct {
	MaxToolCalls   int
	MaxSpendUSD    float64
	MaxEgressBytes int64
}

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
		cap = 30
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

// BudgetState is a snapshot of consumed resources. Returned on
// every decision and on a stand-alone State() call so the agent
// loop can surface mid-turn totals to the user.
type BudgetState struct {
	ToolCalls   int
	SpendUSD    float64
	EgressBytes int64
}

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

// RecordToolCall increments the tool-call counter and returns a
// decision. Called by the agent loop BEFORE dispatching a tool
// invocation; if Exceeded, the loop returns require_confirmation
// without invoking the tool.
func (b *TurnBudget) RecordToolCall() BudgetDecision {
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
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spendUSD += rec.CostUSD
	b.records = append(b.records, rec)
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
	b.mu.Lock()
	defer b.mu.Unlock()
	b.egressBytes += n
	if b.caps.MaxEgressBytes > 0 && b.egressBytes > b.caps.MaxEgressBytes {
		return b.exceededLocked("egress")
	}
	return b.withinLocked()
}

// Check returns the current decision without incrementing anything.
// Agent loop uses this to peek at state mid-turn for the user-
// facing "you've spent $0.42 of your $1.00 budget" display.
func (b *TurnBudget) Check() BudgetDecision {
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
func (b *TurnBudget) Caps() BudgetCaps { return b.caps }

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
