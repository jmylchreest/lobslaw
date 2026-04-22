package types

// TurnBudget bounds resource usage within a single agent turn.
// Exceeding any dimension raises require_confirmation.
type TurnBudget struct {
	MaxToolCalls   int     `json:"max_tool_calls"`
	MaxSpendUSD    float64 `json:"max_spend_usd"`
	MaxEgressBytes int64   `json:"max_egress_bytes"`
}

type TurnUsage struct {
	ToolCalls   int     `json:"tool_calls"`
	SpendUSD    float64 `json:"spend_usd"`
	EgressBytes int64   `json:"egress_bytes"`
}

// Exceeded reports whether any dimension of usage has reached or
// exceeded its budget. A zero-value budget field disables that
// dimension's check.
func (u TurnUsage) Exceeded(b TurnBudget) bool {
	if b.MaxToolCalls > 0 && u.ToolCalls >= b.MaxToolCalls {
		return true
	}
	if b.MaxSpendUSD > 0 && u.SpendUSD >= b.MaxSpendUSD {
		return true
	}
	if b.MaxEgressBytes > 0 && u.EgressBytes >= b.MaxEgressBytes {
		return true
	}
	return false
}
