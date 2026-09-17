package compute

import (
	"context"
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

	// parent is the reservation this budget draws from, set by Sub
	// and nil for a top-level turn. Immutable after construction, so
	// it is read without the mutex.
	//
	// It exists because a fan-out that hands every child its own
	// fresh TurnBudget bounds each child and bounds the RUN not at
	// all: N children times a per-child cap is a spend multiplier
	// wearing a limit's clothing, and no single object knows the
	// total. A child that reports to a parent makes the aggregate
	// the thing that is actually enforced.
	parent *TurnBudget

	mu          sync.Mutex
	toolCalls   int
	spendUSD    float64
	tokens      int64
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

// BudgetState is a snapshot of consumed resources. Returned on
// every decision and on a stand-alone State() call so the agent
// loop can surface mid-turn totals to the user.
type BudgetState struct {
	ToolCalls   int
	SpendUSD    float64
	EgressBytes int64
	// Tokens is the running total across every token-billed call in
	// the turn. Surfaced because spend alone is unreadable on a plan
	// that bills a flat rate — the cost stays zero while the usage
	// that will eventually exhaust the plan goes unrecorded.
	Tokens int64
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

// Sub returns a child budget that draws from b.
//
// Every Record on the child is also recorded against b, so however
// many children a fan-out creates they cannot collectively exceed the
// parent's caps. The child's own caps bound ONE child's share — that
// is what stops a single bad worker starving its siblings — but they
// are a fairness bound, not the spend bound. The parent is the spend
// bound, and it is the one that adds up.
//
// A child's caps are not clamped to the parent's. They do not need to
// be: a child asking for more than the reservation holds still gets
// Exceeded from the parent on the call that crosses it, and clamping
// would silently rewrite a caller's stated share into something it
// did not ask for.
func (b *TurnBudget) Sub(caps BudgetCaps) (*TurnBudget, error) {
	child, err := NewTurnBudget(caps)
	if err != nil {
		return nil, err
	}
	child.parent = b
	return child, nil
}

// drawOn folds a parent's decision into the child's own.
//
// A child that has already refused does not draw on the reservation.
// Callers check the budget BEFORE dispatching and skip the call when
// it says Exceeded, so a refused call never happens and must not cost
// the run — charging for it leaks the reservation to workers that did
// no work, and the deeper the fan-out the more it leaks.
//
// Otherwise the parent decides. Its verdict wins, because it is the
// real bound and a child reporting its own comfortable "within" would
// hide it; the child's counters stay in Current because a caller wants
// to know what IT spent, not what the whole fan-out did.
func (b *TurnBudget) drawOn(own BudgetDecision, record func(*TurnBudget) BudgetDecision) BudgetDecision {
	if b.parent == nil || own.Exceeded {
		return own
	}
	parent := record(b.parent)
	if parent.Exceeded {
		parent.Current = own.Current
		return parent
	}
	return own
}

// RecordToolCall increments the tool-call counter and returns a
// decision. Called by the agent loop BEFORE dispatching a tool
// invocation; if Exceeded, the loop returns require_confirmation
// without invoking the tool.
func (b *TurnBudget) RecordToolCall() BudgetDecision {
	return b.drawOn(b.recordToolCallLocal(), (*TurnBudget).RecordToolCall)
}

func (b *TurnBudget) recordToolCallLocal() BudgetDecision {
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
	return b.drawOn(b.recordCostUSDLocal(rec), func(p *TurnBudget) BudgetDecision {
		return p.RecordCostUSD(rec)
	})
}

func (b *TurnBudget) recordCostUSDLocal(rec CostRecord) BudgetDecision {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spendUSD += rec.CostUSD
	// Tokens only when the charge is actually token-billed. A
	// per-second video charge has a Quantity too, and adding it to a
	// token count would produce a number that means nothing.
	if rec.Usage.Unit == UnitTokens && rec.Usage.Tokens != nil {
		b.tokens += int64(rec.Usage.Tokens.TotalTokens)
	}
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
	return b.drawOn(b.recordEgressBytesLocal(n), func(p *TurnBudget) BudgetDecision {
		return p.RecordEgressBytes(n)
	})
}

func (b *TurnBudget) recordEgressBytesLocal(n int64) BudgetDecision {
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
	return b.drawOn(b.checkLocal(), (*TurnBudget).Check)
}

func (b *TurnBudget) checkLocal() BudgetDecision {
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

// Tighten narrows this budget's caps, never widens them.
//
// Needed because a channel builds its budget before anything knows
// which bot is taking the turn: Telegram, Slack, REST and the inbound
// webhook all construct one from the node default and hand it to the
// agent, and the bot is only resolved inside. Without this a bot's
// caps applied on exactly one of the five paths — the console, which
// goes through TurnRunner — and a bot you had deliberately restricted
// spent the node's full allowance everywhere else.
//
// Only ever downward. mergeCaps already clamps a bot asking for MORE
// than the node allows, and the guard here is the same idea at a
// different moment: a turn already under way must not be able to buy
// itself room.
func (b *TurnBudget) Tighten(caps BudgetCaps) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.caps = mergeCaps(b.caps, caps)
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
	// Forward-only for the same reason as the rest: a resumed turn
	// must not be able to under-report what it has already used.
	if state.Tokens > b.tokens {
		b.tokens = state.Tokens
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
//
// On a child from Sub this lifts that child's fairness share and
// nothing else — the reservation it draws from is untouched. Relaxing
// your way out of a shared bound by approving one worker would defeat
// the reservation for every sibling that had not asked.
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
		Tokens:      b.tokens,
		EgressBytes: b.egressBytes,
	}
}

// budgetKey carries the turn's budget on the context.
type budgetKey struct{}

// WithBudget attaches a turn's budget so a builtin that starts a
// CHILD turn can make it draw on the same reservation.
//
// On the context rather than in the tool-argument map, for the reason
// turn.Identity is: that map is built from the model's own JSON
// output, so anything read out of it is something the model can
// choose — and a model that could choose its own budget has none.
func WithBudget(ctx context.Context, b *TurnBudget) context.Context {
	if b == nil {
		return ctx
	}
	return context.WithValue(ctx, budgetKey{}, b)
}

// BudgetFrom returns the turn's budget, or nil when nothing attached
// one. Nil is usable: a child with no reservation gets a fresh budget
// from config, which is the behaviour before delegation existed.
func BudgetFrom(ctx context.Context) *TurnBudget {
	b, _ := ctx.Value(budgetKey{}).(*TurnBudget)
	return b
}
