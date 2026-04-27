package memory

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// MergeResult carries the post-merge counts. Populated per Dream
// pass and surfaced via DreamResult so operators can see how much
// consolidation happened.
type MergeResult struct {
	Merged     int // clusters where Adjudicator said "merge"
	Conflicts  int // clusters tagged as contradictions
	Supersedes int // clusters tagged as supersession chains
}

// mergePhase runs the Dream-time consolidation-merge pass. Steps:
//
//  1. FindClusters over long-term records (only). Session/episodic
//     records are meant to decay via retention, not auto-consolidate.
//  2. For each cluster, the Adjudicator (LLM or stub) renders a
//     verdict.
//  3. Execute the verdict:
//     - Merge: store a new consolidated VectorRecord + delete sources.
//     - Conflict: tag all members with conflict-cluster:<id>.
//     - Supersedes: tag all members with supersedes-chain:<id>.
//     - KeepDistinct: no-op (default on any failure).
//
// Runs off the leader path — callers already check d.raft.IsLeader
// before invoking Run. An Adjudicator failure for a single cluster
// is logged and skipped; the phase continues with the next cluster.
//
// Returns the MergeResult counts; individual cluster failures are
// non-fatal to the overall phase.
func (d *DreamRunner) mergePhase(ctx context.Context) (MergeResult, error) {
	if d.adjudicator == nil {
		return MergeResult{}, nil
	}
	clusters, err := findClusters(d.store, clusterQuery{
		threshold:       defaultClusterThreshold,
		retentionFilter: lobslawv1.Retention_RETENTION_LONG_TERM,
	})
	if err != nil {
		return MergeResult{}, fmt.Errorf("find clusters: %w", err)
	}

	var result MergeResult
	for _, c := range clusters {
		decision, err := d.adjudicator.AdjudicateMerge(ctx, c)
		if err != nil {
			// Conservative: LLM failure is NEVER destructive. Log and
			// keep the cluster intact — next Dream run may succeed.
			d.logger.Warn("merge: adjudicate failed, keeping cluster distinct",
				"cluster", c.Id, "members", len(c.Records), "err", err)
			continue
		}
		d.logger.Info("merge verdict",
			"cluster", c.Id,
			"verdict", decision.Verdict.String(),
			"reason", decision.Reason,
			"members", len(c.Records),
			"avg_similarity", c.AvgSimilarity,
		)

		switch decision.Verdict {
		case MergeVerdictMerge:
			if err := d.applyMerge(c, decision); err != nil {
				d.logger.Warn("merge: apply failed", "cluster", c.Id, "err", err)
				continue
			}
			result.Merged++
		case MergeVerdictConflict:
			if err := d.tagCluster(c, "conflict-cluster", c.Id); err != nil {
				d.logger.Warn("merge: tag failed", "cluster", c.Id, "err", err)
				continue
			}
			result.Conflicts++
		case MergeVerdictSupersedes:
			if err := d.tagCluster(c, "supersedes-chain", c.Id); err != nil {
				d.logger.Warn("merge: tag failed", "cluster", c.Id, "err", err)
				continue
			}
			result.Supersedes++
		case MergeVerdictKeepDistinct:
			// by design — no action taken, no data lost.
		}
	}
	return result, nil
}

// applyMerge consolidates a cluster into one VectorRecord and
// deletes the originals. Through Raft — FSM replication is what
// makes the change durable and visible on all peers.
//
// The consolidated record:
//   - Id: stable "merged-<cluster.Id>"
//   - Text: decision.MergedText (the LLM's canonical form)
//   - Embedding: centroid of source embeddings (cheap; the
//     consolidation isn't being indexed for precise recall,
//     it's the "representative" vector for the merged concept)
//   - SourceIds: IDs of all originals — cascade logic still works
//   - Retention: highest retention among sources (never downgrade)
func (d *DreamRunner) applyMerge(c *lobslawv1.Cluster, decision MergeDecision) error {
	if len(c.Records) == 0 {
		return fmt.Errorf("empty cluster")
	}

	sourceIDs := make([]string, len(c.Records))
	retentions := make([]lobslawv1.Retention, 0, len(c.Records))
	for i, r := range c.Records {
		sourceIDs[i] = r.Id
		retentions = append(retentions, r.Retention)
	}

	centroid := computeCentroid(c.Records)
	now := d.cfg.Now()

	consolidated := &lobslawv1.VectorRecord{
		Id:        "merged-" + c.Id,
		Embedding: centroid,
		Text:      decision.MergedText,
		Retention: highestRetention(retentions),
		SourceIds: sourceIDs,
		CreatedAt: timestamppb.New(now),
		Scope:     c.Records[0].Scope,
	}

	if err := d.applyEntry(&lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      consolidated.Id,
		Payload: &lobslawv1.LogEntry_VectorRecord{VectorRecord: consolidated},
	}); err != nil {
		return fmt.Errorf("put consolidated: %w", err)
	}

	// Delete sources. Not atomic with the PUT above — if we crash
	// between, next Dream run sees an orphaned consolidated record
	// pointing at live sources. That's OK: sources remain valid and
	// a retry will finish the deletes. Idempotent.
	for _, srcID := range sourceIDs {
		if err := d.applyEntry(&lobslawv1.LogEntry{
			Op:      lobslawv1.LogOp_LOG_OP_DELETE,
			Id:      srcID,
			Payload: &lobslawv1.LogEntry_VectorRecord{VectorRecord: &lobslawv1.VectorRecord{Id: srcID}},
		}); err != nil {
			return fmt.Errorf("delete source %q: %w", srcID, err)
		}
	}
	return nil
}

// tagCluster adds a metadata tag to every VectorRecord in the
// cluster so future Recalls can surface the grouping (e.g. "these
// three records contradict each other" via a UI that checks
// metadata["conflict-cluster"]).
//
// Re-reads each record before writing to avoid clobbering any
// concurrent changes to its metadata by another writer; safe because
// the read-modify-write happens under Raft's linearised apply
// pipeline. The tag key/value is prefixed ("conflict-cluster") +
// the cluster ID.
func (d *DreamRunner) tagCluster(c *lobslawv1.Cluster, tagKey, tagValue string) error {
	for _, r := range c.Records {
		raw, err := d.store.Get(BucketVectorRecords, r.Id)
		if err != nil {
			return fmt.Errorf("read %q: %w", r.Id, err)
		}
		if raw == nil {
			// Deleted since cluster was computed — skip, not fatal.
			continue
		}
		var current lobslawv1.VectorRecord
		if err := proto.Unmarshal(raw, &current); err != nil {
			return fmt.Errorf("unmarshal %q: %w", r.Id, err)
		}
		if current.Metadata == nil {
			current.Metadata = make(map[string]string, 1)
		}
		if existing, ok := current.Metadata[tagKey]; ok && existing == tagValue {
			continue
		}
		current.Metadata[tagKey] = tagValue

		if err := d.applyEntry(&lobslawv1.LogEntry{
			Op:      lobslawv1.LogOp_LOG_OP_PUT,
			Id:      current.Id,
			Payload: &lobslawv1.LogEntry_VectorRecord{VectorRecord: &current},
		}); err != nil {
			return fmt.Errorf("tag %q with %s=%s: %w", r.Id, tagKey, tagValue, err)
		}
	}
	return nil
}

// computeCentroid returns the element-wise mean of the record
// embeddings. Assumes same-dimension embeddings (enforced by
// findClusters — mixed-dim records are filtered before clustering).
// Returns nil if the cluster is empty.
func computeCentroid(records []*lobslawv1.VectorRecord) []float32 {
	if len(records) == 0 || len(records[0].Embedding) == 0 {
		return nil
	}
	dim := len(records[0].Embedding)
	sum := make([]float32, dim)
	count := 0
	for _, r := range records {
		if len(r.Embedding) != dim {
			continue
		}
		for i, v := range r.Embedding {
			sum[i] += v
		}
		count++
	}
	if count == 0 {
		return nil
	}
	for i := range sum {
		sum[i] /= float32(count)
	}
	return sum
}
