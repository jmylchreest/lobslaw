package types

import "time"

// AuditEntry is one immutable record in the tamper-evident audit
// chain. The same struct is written to both sinks (Raft + local
// JSONL). PrevHash preserves chain integrity across local-sink
// log rotation: the final hash of the old file becomes the first
// PrevHash of the new file.
type AuditEntry struct {
	ID         string    `json:"id"`
	Timestamp  time.Time `json:"ts"`
	ActorScope string    `json:"actor_scope"`
	Action     string    `json:"action"`
	Target     string    `json:"target"`
	Argv       []string  `json:"argv,omitempty"`
	PolicyRule string    `json:"policy_rule"`
	Effect     Effect    `json:"effect"`
	ResultHash string    `json:"result_hash,omitempty"`
	PrevHash   string    `json:"prev_hash"`
}

type AuditFilter struct {
	ActorScope string    `json:"actor_scope,omitempty"`
	Action     string    `json:"action,omitempty"`
	Target     string    `json:"target,omitempty"`
	Since      time.Time `json:"since,omitempty"`
	Until      time.Time `json:"until,omitempty"`
	Limit      int       `json:"limit,omitempty"`
}
