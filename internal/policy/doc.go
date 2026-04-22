// Package policy implements the policy engine and rule store. Rules
// live in the shared Raft group; each node enforces locally against
// its cached rule set. Effects: allow, deny, require_confirmation.
package policy
