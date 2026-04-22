// Package memory implements the memory node: vector embeddings,
// episodic records, retention-tagged storage, dream/REM consolidation,
// and the Forget cascade. Persisted on hashicorp/raft + bbolt, with
// application-state values encrypted via pkg/crypto.
package memory
