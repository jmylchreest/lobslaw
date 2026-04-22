package types

// RiskTier classifies how reversible a tool invocation is. Policy
// defaults to require_confirmation for irreversible and
// communicating-to-untrusted, allow for reversible.
type RiskTier string

const (
	RiskReversible    RiskTier = "reversible"
	RiskCommunicating RiskTier = "communicating"
	RiskIrreversible  RiskTier = "irreversible"
)

func (r RiskTier) IsValid() bool {
	switch r {
	case RiskReversible, RiskCommunicating, RiskIrreversible:
		return true
	}
	return false
}
