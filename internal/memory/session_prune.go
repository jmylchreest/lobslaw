package memory

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// SessionPruneConfig tunes the session retention pruner.
type SessionPruneConfig struct {
	// MaxAge is how long a retention=session record lives before it
	// becomes a prune candidate. Default 24h.
	MaxAge time.Duration
	// Now is the wall-clock function for tests.
	Now func() time.Time
}

// SessionPruner hard-deletes records with Retention=session whose
// timestamp is older than MaxAge. Distinct from Dream/REM:
//   - dream consolidates (many → one summary)
//   - session prune deletes outright (no summary, no cascade)
//
// Session-tagged records are by definition transient — the agent
// records them knowing they'll evaporate. Hard delete is therefore
// safe: no SourceIds chains to walk, no archival to keep.
type SessionPruner struct {
	store  *Store
	raft   *RaftNode
	cfg    SessionPruneConfig
	logger *slog.Logger
}

// NewSessionPruner constructs a pruner. logger nil → slog.Default.
// MaxAge zero → 24h.
func NewSessionPruner(raft *RaftNode, store *Store, cfg SessionPruneConfig, logger *slog.Logger) *SessionPruner {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = 24 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &SessionPruner{store: store, raft: raft, cfg: cfg, logger: logger}
}

// PruneResult is the outcome of a single Run.
type PruneResult struct {
	EpisodicPruned int
	VectorPruned   int
}

// Run scans both record buckets, deletes anything tagged
// retention=session whose age exceeds cfg.MaxAge. Leader-only —
// non-leaders return (nil, nil) as a soft skip so a parallel claim
// from another node doesn't double-fire deletes.
func (p *SessionPruner) Run(ctx context.Context) (*PruneResult, error) {
	if p.raft != nil && !p.raft.IsLeader() {
		p.logger.Debug("session-prune: not leader, skipping")
		return nil, nil
	}

	now := p.cfg.Now()
	cutoff := now.Add(-p.cfg.MaxAge)

	type victim struct {
		id     string
		bucket string
	}
	var victims []victim

	if err := p.store.ForEach(BucketEpisodicRecords, func(id string, value []byte) error {
		var rec lobslawv1.EpisodicRecord
		if err := proto.Unmarshal(value, &rec); err != nil {
			return fmt.Errorf("unmarshal episodic %q: %w", id, err)
		}
		if rec.Retention != string(types.RetentionSession) {
			return nil
		}
		if rec.Timestamp == nil || !rec.Timestamp.AsTime().Before(cutoff) {
			return nil
		}
		victims = append(victims, victim{id: id, bucket: BucketEpisodicRecords})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan episodic: %w", err)
	}

	if err := p.store.ForEach(BucketVectorRecords, func(id string, value []byte) error {
		var rec lobslawv1.VectorRecord
		if err := proto.Unmarshal(value, &rec); err != nil {
			return fmt.Errorf("unmarshal vector %q: %w", id, err)
		}
		if rec.Retention != string(types.RetentionSession) {
			return nil
		}
		if rec.CreatedAt == nil || !rec.CreatedAt.AsTime().Before(cutoff) {
			return nil
		}
		victims = append(victims, victim{id: id, bucket: BucketVectorRecords})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan vector: %w", err)
	}

	result := &PruneResult{}
	for _, v := range victims {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		var entry *lobslawv1.LogEntry
		switch v.bucket {
		case BucketEpisodicRecords:
			entry = &lobslawv1.LogEntry{
				Op:      lobslawv1.LogOp_LOG_OP_DELETE,
				Id:      v.id,
				Payload: &lobslawv1.LogEntry_EpisodicRecord{EpisodicRecord: &lobslawv1.EpisodicRecord{Id: v.id}},
			}
		case BucketVectorRecords:
			entry = &lobslawv1.LogEntry{
				Op:      lobslawv1.LogOp_LOG_OP_DELETE,
				Id:      v.id,
				Payload: &lobslawv1.LogEntry_VectorRecord{VectorRecord: &lobslawv1.VectorRecord{Id: v.id}},
			}
		}
		data, err := proto.Marshal(entry)
		if err != nil {
			return result, fmt.Errorf("marshal delete %q: %w", v.id, err)
		}
		if _, err := p.raft.Apply(data, applyTimeout); err != nil {
			return result, fmt.Errorf("apply delete %q: %w", v.id, err)
		}
		switch v.bucket {
		case BucketEpisodicRecords:
			result.EpisodicPruned++
		case BucketVectorRecords:
			result.VectorPruned++
		}
	}

	p.logger.Info("session-prune complete",
		"episodic_pruned", result.EpisodicPruned,
		"vector_pruned", result.VectorPruned,
		"max_age", p.cfg.MaxAge,
		"cutoff", cutoff.Format(time.RFC3339),
	)
	return result, nil
}
