package compute

import (
	"errors"
	"sync"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestNewTurnBudgetRejectsNegativeCaps(t *testing.T) {
	t.Parallel()
	cases := []BudgetCaps{
		{MaxToolCalls: -1},
		{MaxSpendUSD: -0.01},
		{MaxEgressBytes: -1},
	}
	for _, c := range cases {
		if _, err := NewTurnBudget(c); !errors.Is(err, ErrBudgetConfigInvalid) {
			t.Errorf("caps %+v: want ErrBudgetConfigInvalid; got %v", c, err)
		}
	}
}

func TestNewTurnBudgetZeroIsUnlimited(t *testing.T) {
	t.Parallel()
	b, err := NewTurnBudget(BudgetCaps{}) // all zero
	if err != nil {
		t.Fatalf("zero caps should be valid: %v", err)
	}
	// Burn lots of everything — no decision should ever be Exceeded.
	for range 100 {
		if d := b.RecordToolCall(); d.Exceeded {
			t.Error("zero MaxToolCalls should be unlimited")
		}
	}
	if d := b.RecordCostUSD(CostRecord{CostUSD: 1_000_000}); d.Exceeded {
		t.Error("zero MaxSpendUSD should be unlimited")
	}
}

func TestRecordToolCallExceeded(t *testing.T) {
	t.Parallel()
	b, _ := NewTurnBudget(BudgetCaps{MaxToolCalls: 2})
	if d := b.RecordToolCall(); d.Exceeded {
		t.Error("1st call within; got exceeded")
	}
	if d := b.RecordToolCall(); d.Exceeded {
		t.Error("2nd call at cap; should still be within (cap inclusive)")
	}
	d := b.RecordToolCall()
	if !d.Exceeded {
		t.Error("3rd call over cap; expected exceeded")
	}
	if d.ExceededOn != "tool_calls" {
		t.Errorf("ExceededOn: got %q, want 'tool_calls'", d.ExceededOn)
	}
}

func TestRecordCostUSDExceeded(t *testing.T) {
	t.Parallel()
	b, _ := NewTurnBudget(BudgetCaps{MaxSpendUSD: 0.10})
	_ = b.RecordCostUSD(CostRecord{CostUSD: 0.05})
	_ = b.RecordCostUSD(CostRecord{CostUSD: 0.04})
	d := b.RecordCostUSD(CostRecord{CostUSD: 0.02}) // total 0.11 > 0.10
	if !d.Exceeded {
		t.Error("over-cap record should be exceeded")
	}
	if d.ExceededOn != "spend" {
		t.Errorf("ExceededOn: got %q", d.ExceededOn)
	}
}

func TestRecordEgressBytesExceeded(t *testing.T) {
	t.Parallel()
	b, _ := NewTurnBudget(BudgetCaps{MaxEgressBytes: 1024})
	_ = b.RecordEgressBytes(500)
	d := b.RecordEgressBytes(600) // total 1100 > 1024
	if !d.Exceeded {
		t.Error("over-cap egress should be exceeded")
	}
	if d.ExceededOn != "egress" {
		t.Errorf("ExceededOn: got %q", d.ExceededOn)
	}
}

// TestIndependentDimensionsDontCrossExceed — exceeding tool-call
// cap doesn't falsely report a spend exceed (and vice versa).
func TestIndependentDimensionsDontCrossExceed(t *testing.T) {
	t.Parallel()
	b, _ := NewTurnBudget(BudgetCaps{MaxToolCalls: 1, MaxSpendUSD: 10.0})
	_ = b.RecordToolCall()
	d := b.RecordToolCall()
	if !d.Exceeded {
		t.Fatal("should exceed on tool_calls")
	}
	if d.ExceededOn != "tool_calls" {
		t.Errorf("expected tool_calls; got %q", d.ExceededOn)
	}
}

func TestBudgetStateSnapshot(t *testing.T) {
	t.Parallel()
	b, _ := NewTurnBudget(BudgetCaps{})
	_ = b.RecordToolCall()
	_ = b.RecordToolCall()
	_ = b.RecordCostUSD(CostRecord{CostUSD: 0.25})
	_ = b.RecordEgressBytes(512)

	state := b.State()
	if state.ToolCalls != 2 {
		t.Errorf("ToolCalls: %d", state.ToolCalls)
	}
	if state.SpendUSD != 0.25 {
		t.Errorf("SpendUSD: %f", state.SpendUSD)
	}
	if state.EgressBytes != 512 {
		t.Errorf("EgressBytes: %d", state.EgressBytes)
	}
}

func TestBudgetCheckDoesNotIncrement(t *testing.T) {
	t.Parallel()
	b, _ := NewTurnBudget(BudgetCaps{MaxToolCalls: 1})
	_ = b.RecordToolCall()
	// Repeated Check() should always report the same state.
	for range 5 {
		if d := b.Check(); d.Exceeded {
			t.Error("Check should not push over cap")
		}
	}
	if b.State().ToolCalls != 1 {
		t.Errorf("Check leaked into counter: state=%+v", b.State())
	}
}

func TestBudgetRecordsAccumulate(t *testing.T) {
	t.Parallel()
	b, _ := NewTurnBudget(BudgetCaps{})
	r1 := CostRecord{ProviderLabel: "a", Model: "m", Usage: Usage{PromptTokens: 100}, CostUSD: 0.01}
	r2 := CostRecord{ProviderLabel: "b", Model: "m2", Usage: Usage{PromptTokens: 200}, CostUSD: 0.02}
	_ = b.RecordCostUSD(r1)
	_ = b.RecordCostUSD(r2)

	records := b.Records()
	if len(records) != 2 {
		t.Fatalf("want 2 records, got %d", len(records))
	}
	if records[0].ProviderLabel != "a" || records[1].ProviderLabel != "b" {
		t.Errorf("record order lost: %+v", records)
	}
}

func TestBudgetRecordsAreDefensiveCopy(t *testing.T) {
	t.Parallel()
	b, _ := NewTurnBudget(BudgetCaps{})
	_ = b.RecordCostUSD(CostRecord{ProviderLabel: "orig"})
	r := b.Records()
	r[0].ProviderLabel = "mutated"
	// Fresh read should still say "orig".
	if b.Records()[0].ProviderLabel != "orig" {
		t.Error("external slice mutation leaked into budget's records")
	}
}

func TestFromConfigMapping(t *testing.T) {
	t.Parallel()
	caps := FromConfig(config.BudgetsConfig{
		MaxToolCallsPerTurn:   5,
		MaxSpendUSDPerTurn:    0.50,
		MaxEgressBytesPerTurn: 1024 * 1024,
	})
	if caps.MaxToolCalls != 5 || caps.MaxSpendUSD != 0.50 || caps.MaxEgressBytes != 1024*1024 {
		t.Errorf("field mapping wrong: %+v", caps)
	}
}

func TestConcurrentAccessIsSafe(t *testing.T) {
	t.Parallel()
	b, _ := NewTurnBudget(BudgetCaps{})
	const n = 100
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for range n {
			_ = b.RecordToolCall()
		}
	}()
	go func() {
		defer wg.Done()
		for range n {
			_ = b.RecordCostUSD(CostRecord{CostUSD: 0.01})
		}
	}()
	go func() {
		defer wg.Done()
		for range n {
			_ = b.RecordEgressBytes(1)
		}
	}()
	wg.Wait()

	state := b.State()
	if state.ToolCalls != n {
		t.Errorf("ToolCalls lost: got %d, want %d", state.ToolCalls, n)
	}
	if state.EgressBytes != int64(n) {
		t.Errorf("EgressBytes lost: got %d, want %d", state.EgressBytes, n)
	}
}

// TestCapsReturnsOperatorConfig — the caller (agent loop) needs
// the original caps to render "you've spent X of Y" UI.
func TestCapsReturnsOperatorConfig(t *testing.T) {
	t.Parallel()
	caps := BudgetCaps{MaxToolCalls: 10, MaxSpendUSD: 1.0}
	b, _ := NewTurnBudget(caps)
	if b.Caps() != caps {
		t.Errorf("Caps() should round-trip; got %+v", b.Caps())
	}
}

// TestBudgetSmokeWithRealCostRecord confirms the integration
// between pricing.EstimateCost and budget: computing a cost the
// usual way then recording it should arrive at the expected total.
func TestBudgetSmokeWithRealCostRecord(t *testing.T) {
	t.Parallel()
	b, _ := NewTurnBudget(BudgetCaps{MaxSpendUSD: 1.0})

	pricing := types.ProviderPricing{InputUSDPer1K: 0.01, OutputUSDPer1K: 0.02}
	rec := RecordCost("p", "m", Usage{PromptTokens: 1000, CompletionTokens: 500}, pricing)
	// Expected: 1000*0.01/1000 + 500*0.02/1000 = 0.01 + 0.01 = 0.02.

	d := b.RecordCostUSD(rec)
	if d.Exceeded {
		t.Error("$0.02 should fit in $1 budget")
	}
	state := b.State()
	if state.SpendUSD < 0.019 || state.SpendUSD > 0.021 {
		t.Errorf("state.SpendUSD should be ~0.02; got %f", state.SpendUSD)
	}
}
