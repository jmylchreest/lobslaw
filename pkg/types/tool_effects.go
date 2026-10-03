package types

// ToolEffects separates state access from transport. A read over the network
// remains a read; RiskTier alone cannot express that distinction.
type ToolEffects struct {
	// Reads marks a separate read effect in an otherwise mutating operation.
	Reads   bool   `json:"reads,omitempty"`
	State   string `json:"state"`
	Network bool   `json:"network"`
}

// State access categories used by trusted tool effect declarations.
const (
	ToolReads   = "read"
	ToolWrites  = "write"
	ToolDeletes = "delete"
)

// Valid reports whether the declaration has a recognized state category.
func (e ToolEffects) Valid() bool {
	return e.State == ToolReads || e.State == ToolWrites || e.State == ToolDeletes
}
