package types

import "time"

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

	// CreatedBy and CreatedAt carry a rule's provenance. They are not
	// evaluated; they exist so a listing can tell an operator-authored
	// rule (empty CreatedBy) from one minted by an "always" approval
	// ("approval:<prompt_id>"), which is what makes such a grant
	// reviewable and revocable.
	//
	// Both must survive the proto round trip. Without them here the
	// wire type's created_by is silently dropped on the way through
	// this struct, which is how `policy approvals --created-by` and
	// the created_by column came to report nothing on a live node.
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
}

// Condition is an additional predicate that must hold for the rule
// to apply. Expand the set of supported ops deliberately.
type Condition struct {
	Key   string `json:"key"`
	Op    string `json:"op"`
	Value string `json:"value"`
}
