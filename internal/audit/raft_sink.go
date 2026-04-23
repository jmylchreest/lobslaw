package audit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// RaftSink persists entries to the replicated audit_entries bucket
// via Raft consensus. Every voter applies the same log entry; a
// compromised node trying to hide its own activity from the Raft
// chain can't (every other node sees the commit). Defence-in-depth
// partner to LocalSink.
type RaftSink struct {
	raft         raftApplier
	store        *memory.Store
	applyTimeout time.Duration
}

// raftApplier is the subset of *memory.RaftNode this sink uses.
// Interface keeps test harnesses from needing a real Raft group.
type raftApplier interface {
	Apply(data []byte, timeout time.Duration) (any, error)
}

// RaftConfig wires the raft + store dependencies + an apply timeout.
// Zero timeout picks 5 seconds.
type RaftConfig struct {
	Raft         raftApplier
	Store        *memory.Store
	ApplyTimeout time.Duration
}

// NewRaftSink constructs a raft-backed sink. Both raft + store are
// required; store is read directly for queries/verify (no need for
// a read round-trip through consensus for read-only paths).
func NewRaftSink(cfg RaftConfig) (*RaftSink, error) {
	if cfg.Raft == nil {
		return nil, errors.New("audit.RaftSink: Raft required")
	}
	if cfg.Store == nil {
		return nil, errors.New("audit.RaftSink: Store required")
	}
	if cfg.ApplyTimeout <= 0 {
		cfg.ApplyTimeout = 5 * time.Second
	}
	return &RaftSink{raft: cfg.Raft, store: cfg.Store, applyTimeout: cfg.ApplyTimeout}, nil
}

// Name satisfies AuditSink.
func (s *RaftSink) Name() string { return "raft" }

// Append serialises the entry as a LogEntry_AuditEntry payload and
// submits via Raft.Apply. Replicates to every voter before
// returning. Failure modes: Raft not leader (operator needs a
// cluster with a quorum), apply timeout (network partition or
// slow follower), FSM rejected payload (shouldn't happen for a
// valid AuditEntry).
func (s *RaftSink) Append(_ context.Context, entry types.AuditEntry) error {
	wire := typedToProto(entry)
	logEntry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: entry.ID,
		Payload: &lobslawv1.LogEntry_AuditEntry{
			AuditEntry: wire,
		},
	}
	data, err := proto.Marshal(logEntry)
	if err != nil {
		return fmt.Errorf("audit.RaftSink: marshal: %w", err)
	}
	res, err := s.raft.Apply(data, s.applyTimeout)
	if err != nil {
		return fmt.Errorf("audit.RaftSink: apply: %w", err)
	}
	if ferr, ok := res.(error); ok && ferr != nil {
		return fmt.Errorf("audit.RaftSink: fsm: %w", ferr)
	}
	return nil
}

// Query reads every audit entry from the local replica's bbolt
// bucket. No index — scan + filter + sort. Realistic for
// personal-scale audit volumes; a multi-GB log would want an
// indexed store. Sorted ascending by timestamp so callers see
// insertion order regardless of bbolt's key-traversal order.
func (s *RaftSink) Query(_ context.Context, filter types.AuditFilter) ([]types.AuditEntry, error) {
	match := filterMatcher(filter)
	var out []types.AuditEntry
	err := s.store.ForEach(memory.BucketAuditEntries, func(_ string, raw []byte) error {
		var wire lobslawv1.AuditEntry
		if err := proto.Unmarshal(raw, &wire); err != nil {
			return err
		}
		e := protoToTyped(&wire)
		if !match(e) {
			return nil
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// VerifyChain walks the bbolt bucket in timestamp-sorted order and
// recomputes the hash chain. Same semantics as LocalSink: first
// broken PrevHash is returned as FirstBreakID. Because this sink
// has no rotation, there's no cross-file boundary to handle.
func (s *RaftSink) VerifyChain(ctx context.Context) (VerifyResult, error) {
	entries, err := s.Query(ctx, types.AuditFilter{})
	if err != nil {
		return VerifyResult{}, err
	}
	var (
		prevHash string
		count    int64
	)
	for _, e := range entries {
		count++
		if e.PrevHash != prevHash {
			return VerifyResult{
				OK:             false,
				FirstBreakID:   e.ID,
				EntriesChecked: count,
			}, nil
		}
		prevHash = ComputeHash(e)
	}
	return VerifyResult{OK: true, EntriesChecked: count}, nil
}

// typedToProto converts the Go-side AuditEntry into the wire shape
// the FSM stores. Kept as a free function so the same conversion
// serves tests that roundtrip without hitting Raft.
func typedToProto(e types.AuditEntry) *lobslawv1.AuditEntry {
	var ts *timestamppb.Timestamp
	if !e.Timestamp.IsZero() {
		ts = timestamppb.New(e.Timestamp)
	}
	argv := make([]string, len(e.Argv))
	copy(argv, e.Argv)
	return &lobslawv1.AuditEntry{
		Id:         e.ID,
		Timestamp:  ts,
		ActorScope: e.ActorScope,
		Action:     e.Action,
		Target:     e.Target,
		Argv:       argv,
		PolicyRule: e.PolicyRule,
		Effect:     string(e.Effect),
		ResultHash: e.ResultHash,
		PrevHash:   e.PrevHash,
	}
}

// protoToTyped is the reverse of typedToProto. Timestamp zero-value
// handling matches the wire-nil case from typedToProto so a round
// trip preserves IsZero().
func protoToTyped(w *lobslawv1.AuditEntry) types.AuditEntry {
	var ts time.Time
	if w.Timestamp != nil {
		ts = w.Timestamp.AsTime()
	}
	argv := make([]string, len(w.Argv))
	copy(argv, w.Argv)
	return types.AuditEntry{
		ID:         w.Id,
		Timestamp:  ts,
		ActorScope: w.ActorScope,
		Action:     w.Action,
		Target:     w.Target,
		Argv:       argv,
		PolicyRule: w.PolicyRule,
		Effect:     types.Effect(w.Effect),
		ResultHash: w.ResultHash,
		PrevHash:   w.PrevHash,
	}
}
