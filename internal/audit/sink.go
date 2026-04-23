package audit

import (
	"context"
	"errors"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// AuditSink is the abstract write/read/verify surface. Two
// production implementations ship in this package: LocalSink
// (JSONL file with lumberjack rotation) and RaftSink
// (Raft-replicated bbolt bucket). An AuditLog coordinator writes
// to every configured sink; reads and verifies scope to one.
type AuditSink interface {
	// Append records the entry. Implementations MUST set no
	// coordinator-owned fields (ID, PrevHash, Timestamp) on the
	// caller's behalf — AuditLog populates those before dispatch.
	Append(ctx context.Context, entry types.AuditEntry) error

	// Query returns entries matching filter in insertion order —
	// oldest first. Implementations should honour Limit as a
	// hard cap; zero means "all matching."
	Query(ctx context.Context, filter types.AuditFilter) ([]types.AuditEntry, error)

	// VerifyChain walks every entry in insertion order, recomputing
	// the hash chain. Returns the ID of the first broken entry
	// (empty when clean), the number of entries checked, and any
	// I/O error. A missing PrevHash link, a content mutation, and
	// a gap across log rotation all look the same from the chain's
	// perspective: first_break_id is set + ok=false.
	VerifyChain(ctx context.Context) (VerifyResult, error)

	// Name returns a short identifier for logs + the AuditService
	// sink-filtering RPC ("raft" | "local"). Kept as a method
	// rather than a struct field so the interface stays narrow.
	Name() string
}

// VerifyResult is the outcome of a chain walk. EntriesChecked
// counts every entry consumed, including the one that broke (if
// any). When a rotation boundary crosses a PrevHash break, the
// ID of the entry with the broken PrevHash is returned, not its
// predecessor.
type VerifyResult struct {
	OK             bool
	FirstBreakID   string
	EntriesChecked int64
}

// ErrSinkClosed surfaces from Append/Query/VerifyChain when a sink
// has been shut down. Callers upstream check for it to distinguish
// "the audit log was torn down" from "the log is corrupt."
var ErrSinkClosed = errors.New("audit: sink closed")
