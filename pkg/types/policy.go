package types

// PolicyRule is one RBAC-style rule. Evaluation: highest priority
// wins; ties break toward deny; no match defaults to deny.
type PolicyRule struct {
	ID         string      `json:"id"`
	Subject    string      `json:"subject"`
	Action     string      `json:"action"`
	Resource   string      `json:"resource"`
	Effect     Effect      `json:"effect"`
	Conditions []Condition `json:"conditions,omitempty"`
	Priority   int         `json:"priority"`
	Scope      string      `json:"scope,omitempty"`
}

// Condition is an additional predicate that must hold for the rule
// to apply. Expand the set of supported ops deliberately.
type Condition struct {
	Key   string `json:"key"`
	Op    string `json:"op"`
	Value string `json:"value"`
}
