package memory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/ids"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Dream-time adjudication: what to do about memories that say nearly
// the same thing.
//
// Runs only when an Adjudicator is wired. Clustering is not free — it
// is O(n²) over the vector bucket — and a pass whose verdicts nothing
// acts on is a nightly cost with no product, which is what the
// previous version of this was.

// MergeOutcome reports what one adjudication pass did.
type MergeOutcome struct {
	Clusters   int
	Merged     int
	Superseded int
	Conflicts  int
	Distinct   int
	// Skipped counts clusters already decided on a previous night.
	Skipped int
}

// mergeMinSimilarity is the floor for calling two records
// near-duplicates.
//
// Higher than the clustering default, because the consequence here is
// destructive where FindClusters' is a report. 0.92 is "said the same
// thing in different words"; the gap down to 0.88 is where "related"
// starts, and related is not duplicate.
const mergeMinSimilarity = 0.92

// mergePhase adjudicates near-duplicate clusters and acts on the
// verdicts.
//
// Long-term only, inherited from the previous design and still right:
// session chatter is pruned on its own schedule, and rewriting it
// would be spending an LLM call on something that will not exist next
// week.
//
// One cluster's failure never ends the pass. An adjudicator that
// errors on one cluster leaves it for tomorrow; a pass that aborted
// would leave every later cluster unexamined for the same reason.
func (d *DreamRunner) mergePhase(ctx context.Context, now time.Time) (MergeOutcome, error) {
	var out MergeOutcome
	if d.adjudicator == nil {
		return out, nil
	}

	clusters, err := findClusters(d.store, clusterQuery{
		threshold:       mergeMinSimilarity,
		retentionFilter: lobslawv1.Retention_RETENTION_LONG_TERM,
		limit:           d.cfg.MaxMergeClusters,
	})
	if err != nil {
		return out, fmt.Errorf("find clusters: %w", err)
	}
	out.Clusters = len(clusters)
	if len(clusters) == 0 {
		return out, nil
	}

	// Already-decided clusters are skipped rather than re-asked.
	// Cluster ids are a hash of their sorted members, so a cluster
	// whose membership has not changed is the same question — and
	// asking a model the same question every night, forever, is how a
	// background pass becomes a standing bill.
	decided, err := adjudicatedClusterIDs(d.store)
	if err != nil {
		return out, fmt.Errorf("read consolidation log: %w", err)
	}

	for _, c := range clusters {
		if decided[c.GetId()] {
			out.Skipped++
			continue
		}
		// An unowned cluster is not adjudicated at all.
		//
		// Records written before ownership existed all carry the
		// empty owner, so they cluster with each other — equal
		// owners, by the letter of the rule that stops cross-owner
		// clustering. Nothing good comes of deciding about them: a
		// merge would refuse for want of an owner, and the refusal
		// happens before the verdict is recorded, so the same cluster
		// would be sent to the model again every night for the life
		// of the node. A conflict would be recorded against no owner,
		// which no principal's nightmare query can see, so the
		// question would be asked of nobody.
		if clusterOwner(c) == "" {
			out.Skipped++
			d.logger.Debug("dream: cluster has no owner; not adjudicated",
				"cluster", c.GetId(), "members", len(c.GetRecords()))
			continue
		}
		adj, err := d.adjudicator.AdjudicateMerge(ctx, c)
		if err != nil {
			d.logger.Warn("dream: adjudication failed; cluster left for the next pass",
				"cluster", c.GetId(), "members", len(c.GetRecords()), "err", err)
			continue
		}
		if adj == nil {
			continue
		}
		if err := d.applyVerdict(ctx, c, adj, now, &out); err != nil {
			d.logger.Warn("dream: verdict could not be applied",
				"cluster", c.GetId(), "verdict", string(adj.Verdict), "err", err)
		}
	}
	return out, nil
}

// applyVerdict turns one decision into writes.
func (d *DreamRunner) applyVerdict(
	ctx context.Context,
	c *lobslawv1.Cluster,
	adj *Adjudication,
	now time.Time,
	out *MergeOutcome,
) error {
	verdict := adj.Verdict
	// A merge with nothing to merge into would delete records in
	// favour of an empty summary. Downgraded rather than refused, so
	// the cluster is still recorded as decided.
	if verdict == VerdictMerge && adj.Consolidated == "" {
		verdict = VerdictKeepDistinct
		adj.Reason = "merge proposed with no consolidated text; kept distinct"
	}
	// Same for a supersedes verdict naming a record outside the
	// cluster: nothing can act on it.
	if verdict == VerdictSupersedes && !clusterHolds(c, adj.Current) {
		verdict = VerdictKeepDistinct
		adj.Reason = "supersedes named a record outside the cluster; kept distinct"
	}

	record := &lobslawv1.ConsolidationRecord{
		Id:            ids.New(),
		ClusterId:     c.GetId(),
		Verdict:       string(verdict),
		Reason:        adj.Reason,
		SourceIds:     episodicIDsOf(c),
		MemberCount:   int32(len(c.GetRecords())),
		AvgSimilarity: c.GetAvgSimilarity(),
		Owner:         clusterOwner(c),
		CreatedAt:     timestamppb.New(now),
	}

	switch verdict {
	case VerdictMerge:
		id, err := d.mergeCluster(ctx, c, adj, now)
		if err != nil {
			return err
		}
		record.ResultId = id
		out.Merged++
	case VerdictSupersedes:
		record.ResultId = adj.Current
		out.Superseded++
	case VerdictConflict:
		out.Conflicts++
	default:
		out.Distinct++
	}

	return d.applyEntry(&lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      record.Id,
		Payload: &lobslawv1.LogEntry_Consolidation{Consolidation: record},
	})
}

// mergeCluster writes the consolidated memory and forgets its
// sources.
//
// Through Remember, like every other memory: the owner comes off the
// cluster, the vector index is written for it, and a merge cannot
// produce the unreadable or unfindable record that hand-assembly
// produced twice before. Sources are deleted only after the
// replacement commits — the other order loses the memory outright if
// the write fails.
func (d *DreamRunner) mergeCluster(
	ctx context.Context,
	c *lobslawv1.Cluster,
	adj *Adjudication,
	now time.Time,
) (string, error) {
	owner := clusterOwner(c)
	if owner == "" {
		return "", errors.New("cluster has no owner")
	}
	sources := episodicIDsOf(c)
	if len(sources) == 0 {
		return "", errors.New("cluster carries no episodic sources")
	}

	merged := &lobslawv1.EpisodicRecord{
		Event:      adj.Consolidated,
		Context:    adj.Consolidated,
		Importance: highestImportance(d.store, sources),
		Timestamp:  timestamppb.New(now),
		Retention:  lobslawv1.Retention_RETENTION_LONG_TERM,
		SourceIds:  sources,
		Owner:      owner,
		Visibility: strictestClusterVisibility(c),
	}
	id, err := Remember(ctx, d.raft, d.embedder, applyTimeout, merged)
	if err != nil {
		return "", fmt.Errorf("write consolidated memory: %w", err)
	}

	// The originals and the vectors that point at them. A vector left
	// behind would keep matching text whose record no longer exists,
	// which recall renders as nothing at all.
	for _, sid := range sources {
		if err := d.forgetEpisodic(sid); err != nil {
			d.logger.Warn("dream: merged record kept its source",
				"source", sid, "merged_into", id, "err", err)
		}
	}
	for _, v := range c.GetRecords() {
		if err := d.applyEntry(&lobslawv1.LogEntry{
			Op:      lobslawv1.LogOp_LOG_OP_DELETE,
			Id:      v.GetId(),
			Payload: &lobslawv1.LogEntry_VectorRecord{VectorRecord: &lobslawv1.VectorRecord{Id: v.GetId()}},
		}); err != nil {
			d.logger.Warn("dream: merged record kept its vector",
				"vector", v.GetId(), "merged_into", id, "err", err)
		}
	}
	return id, nil
}

func (d *DreamRunner) forgetEpisodic(id string) error {
	return d.applyEntry(&lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_DELETE,
		Id:      id,
		Payload: &lobslawv1.LogEntry_EpisodicRecord{EpisodicRecord: &lobslawv1.EpisodicRecord{Id: id}},
	})
}

// adjudicatedClusterIDs is every cluster already decided.
func adjudicatedClusterIDs(store *Store) (map[string]bool, error) {
	out := map[string]bool{}
	err := store.ForEach(BucketConsolidations, func(_ string, raw []byte) error {
		var rec lobslawv1.ConsolidationRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return nil //nolint:nilerr // one unreadable entry must not re-open every cluster
		}
		if rec.ClusterId != "" {
			out[rec.ClusterId] = true
		}
		return nil
	})
	return out, err
}

// episodicIDsOf maps cluster members (vector records) back to the
// memories they index. A vector with no sources is skipped rather
// than guessed at.
func episodicIDsOf(c *lobslawv1.Cluster) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(c.GetRecords()))
	for _, v := range c.GetRecords() {
		for _, sid := range v.GetSourceIds() {
			if sid == "" || seen[sid] {
				continue
			}
			seen[sid] = true
			out = append(out, sid)
		}
	}
	return out
}

// clusterOwner is the owner every member shares. Clustering never
// crosses owners, so disagreement here means the cluster is
// malformed and merging it would mint a record owned by neither.
func clusterOwner(c *lobslawv1.Cluster) string {
	owner := ""
	for _, v := range c.GetRecords() {
		switch {
		case v.GetOwner() == "":
			return ""
		case owner == "":
			owner = v.GetOwner()
		case owner != v.GetOwner():
			return ""
		}
	}
	return owner
}

func clusterHolds(c *lobslawv1.Cluster, episodicID string) bool {
	for _, sid := range episodicIDsOf(c) {
		if sid == episodicID {
			return true
		}
	}
	return false
}

// strictestClusterVisibility keeps the most private setting in the
// cluster. A summary is exactly as readable as the least readable
// thing it summarises.
func strictestClusterVisibility(c *lobslawv1.Cluster) lobslawv1.Visibility {
	strictest := lobslawv1.Visibility_VISIBILITY_UNSPECIFIED
	for _, v := range c.GetRecords() {
		if v.GetVisibility() > strictest {
			strictest = v.GetVisibility()
		}
	}
	return strictest
}

// highestImportance carries the most important source's rating into
// the merge, so consolidating does not quietly demote something.
func highestImportance(store *Store, ids []string) int32 {
	best := int32(0)
	for _, id := range ids {
		raw, err := store.Get(BucketEpisodicRecords, id)
		if err != nil {
			continue
		}
		var rec lobslawv1.EpisodicRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			continue
		}
		if rec.Importance > best {
			best = rec.Importance
		}
	}
	return best
}
