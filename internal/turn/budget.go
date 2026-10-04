package turn

// BudgetCaps are the operator-configured per-turn limits. Zero on
// any field means "no cap" for that dimension.
type BudgetCaps struct {
	MaxToolCalls   int
	MaxSpendUSD    float64
	MaxEgressBytes int64
}

// BudgetState is a snapshot of consumed resources.
type BudgetState struct {
	ToolCalls   int
	SpendUSD    float64
	EgressBytes int64
}
