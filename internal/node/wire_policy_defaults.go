package node

// wirePolicyDefaults composes fallback policy once, after compute registers
// its tools. It also runs on memory-only nodes, which serve upgrade RPCs.
// SetDefaults replaces the whole set: independent subsystem calls would erase
// each other's defaults. Stored policy rules always take precedence.
func (n *Node) wirePolicyDefaults() error {
	defaults := upgradePolicyDefaults()
	if gateCompute(n.cfg) {
		defaults = append(defaults, n.wireApprovalGates()...)
	}
	if n.policyEngine != nil {
		n.policyEngine.SetDefaults(defaults)
	}
	return nil
}
