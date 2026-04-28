package memory

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Summarizer turns a set of source events into a consolidated
// summary text plus an embedding for vector indexing. Phase 3.3
// ships no real implementation — wiring waits for Phase 5's
// Provider Resolver. A nil Summarizer makes Dream skip the
// consolidation step while still running score + prune.
type Summarizer interface {
	Summarize(ctx context.Context, events []string) (summary string, embedding []float32, err error)
}

// DreamConfig tunes Dream/REM behaviour.
type DreamConfig struct {
	// MaxCandidates bounds how many top-scoring records are
	// passed to the Summarizer in one run. Default 10.
	MaxCandidates int

	// PruneThreshold is the minimum score for an episodic record
	// (non-long-term retention only) to survive the prune step.
	// Default 0.1. long-term records are never auto-pruned.
	PruneThreshold float32

	// HalfLife is the recency-decay half-life. Default 14 days —
	// records a half-life old score half as much as fresh ones.
	HalfLife time.Duration

	// CommitmentGrace is the minimum age a fired (status=done)
	// commitment must reach before the dream pass digests it into
	// episodic memory and deletes the original. Default 24h — gives
	// the user a window to ask "what did you remind me about today"
	// against the live commitment listing before history-mode is
	// required.
	CommitmentGrace time.Duration

	// Now is the wall-clock function for tests to override.
	Now func() time.Time
}

// DreamRunner encapsulates the state needed to run a Dream pass.
// Writes go through raft.Apply; reads hit the local store directly.
type DreamRunner struct {
	store       *Store
	raft        *RaftNode
	summarizer  Summarizer  // may be nil until Phase 5
	adjudicator Adjudicator // defaults to AlwaysKeepDistinct — safe no-op
	cfg         DreamConfig
	logger      *slog.Logger
}

// SetSummarizer swaps in the consolidation-layer summarizer.
// Intended for Phase 5 to call after constructing the Provider
// Resolver. Safe to call while the runner is idle.
func (d *DreamRunner) SetSummarizer(s Summarizer) { d.summarizer = s }

// SetAdjudicator swaps in the merge-layer adjudicator. nil is
// normalised to the AlwaysKeepDistinct stub so the merge phase
// always has a non-nil Adjudicator to call (and defaults to
// no-op on startup before Phase 5's LLM module wires in a real one).
func (d *DreamRunner) SetAdjudicator(a Adjudicator) {
	if a == nil {
		a = AlwaysKeepDistinctAdjudicator{}
	}
	d.adjudicator = a
}

// NewDreamRunner constructs a runner. summarizer may be nil — Phase 5
// supplies the real one. The Adjudicator defaults to the always-
// keep-distinct stub, so the merge phase is boot-safe (runs, but
// never merges) until Phase 5 calls SetAdjudicator with an LLM-
// backed implementation.
func NewDreamRunner(raft *RaftNode, store *Store, summarizer Summarizer, cfg DreamConfig, logger *slog.Logger) *DreamRunner {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.MaxCandidates <= 0 {
		cfg.MaxCandidates = 10
	}
	if cfg.PruneThreshold <= 0 {
		cfg.PruneThreshold = 0.1
	}
	if cfg.HalfLife <= 0 {
		cfg.HalfLife = 14 * 24 * time.Hour
	}
	if cfg.CommitmentGrace <= 0 {
		cfg.CommitmentGrace = 24 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &DreamRunner{
		store:       store,
		raft:        raft,
		summarizer:  summarizer,
		adjudicator: AlwaysKeepDistinctAdjudicator{},
		cfg:         cfg,
		logger:      logger,
	}
}

// DreamResult is the outcome of one Dream run.
type DreamResult struct {
	Consolidated int
	Pruned       int
	Candidates   []string // IDs selected for consolidation (may be empty if no Summarizer)
	// Merge is the outcome of the Phase 2 near-duplicate consolidation
	// pass. Zero values are expected before Phase 5 lands — the default
	// AlwaysKeepDistinct Adjudicator never takes destructive action.
	Merge MergeResult
	// CommitmentsDigested is the number of fired commitments swept
	// from BucketCommitments during this pass.
	CommitmentsDigested int
	// CommitmentDigests is the number of per-day rollup EpisodicRecords
	// written into episodic memory by this pass.
	CommitmentDigests int
}

// Run performs one Dream/REM pass: score → select → consolidate (if
// Summarizer wired) → prune → write a dream-session episodic record.
//
// Non-leaders return (nil, nil) as a soft skip — the call is cheap
// and dream is supposed to be idempotent/best-effort.
func (d *DreamRunner) Run(ctx context.Context) (*DreamResult, error) {
	if d.raft != nil && !d.raft.IsLeader() {
		d.logger.Debug("dream: not leader, skipping")
		return nil, nil
	}

	now := d.cfg.Now()

	scored, err := d.scoreAll(now)
	if err != nil {
		return nil, fmt.Errorf("score: %w", err)
	}

	candidates := d.selectTopN(scored, d.cfg.MaxCandidates)

	consolidated := 0
	if d.summarizer != nil && len(candidates) > 0 {
		if err := d.consolidate(ctx, candidates, now); err != nil {
			return nil, fmt.Errorf("consolidate: %w", err)
		}
		consolidated = 1 // one summary per dream pass
	}

	pruned, err := d.prune(scored)
	if err != nil {
		return nil, fmt.Errorf("prune: %w", err)
	}

	// Phase 2: near-duplicate consolidation over long-term records.
	// Runs after prune so session/episodic chatter is already gone
	// and the merge decisions operate on the records that actually
	// matter long-term. LLM failures are non-fatal — the stub
	// Adjudicator is safe, and a real one that errors out just
	// preserves the cluster for next run's retry.
	mergeResult, err := d.mergePhase(ctx)
	if err != nil {
		// Don't fail the whole Run — log and keep the summary/prune
		// results. Next run's mergePhase retries from scratch.
		d.logger.Warn("dream: merge phase failed", "err", err)
	}

	// Commitment digest: roll up fired commitments older than the
	// grace window into per-day episodic summaries, then delete the
	// originals. Keeps BucketCommitments lean for fast listing while
	// preserving the historical fact in vector-searchable form.
	// Failures are non-fatal — log and keep the rest of the pass.
	digested, digests, err := d.digestCommitments(now)
	if err != nil {
		d.logger.Warn("dream: commitment digest failed", "err", err)
	}

	if err := d.logDreamSession(now, len(candidates), consolidated, pruned, digested, digests); err != nil {
		// Don't fail the run for a log-entry error; just warn.
		d.logger.Warn("dream: failed to log session", "err", err)
	}

	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.id)
	}
	result := &DreamResult{
		Consolidated:        consolidated,
		Pruned:              pruned,
		Candidates:          ids,
		Merge:               mergeResult,
		CommitmentsDigested: digested,
		CommitmentDigests:   digests,
	}
	d.logger.Info("dream complete",
		"candidates", len(ids),
		"consolidated", consolidated,
		"pruned", pruned,
		"merged", mergeResult.Merged,
		"conflicts", mergeResult.Conflicts,
		"supersedes", mergeResult.Supersedes,
		"commitments_digested", digested,
		"commitment_digests", digests,
	)
	return result, nil
}

// scoredRecord is an internal carrier that remembers enough about
// each episodic record to sort, prune, or pass into consolidation
// without re-reading it from the store.
type scoredRecord struct {
	id        string
	record    *lobslawv1.EpisodicRecord
	score     float32
	retention lobslawv1.Retention
}

func (d *DreamRunner) scoreAll(now time.Time) ([]scoredRecord, error) {
	halfLifeSecs := float64(d.cfg.HalfLife.Seconds())
	var out []scoredRecord
	err := d.store.ForEach(BucketEpisodicRecords, func(id string, value []byte) error {
		var rec lobslawv1.EpisodicRecord
		if err := proto.Unmarshal(value, &rec); err != nil {
			return fmt.Errorf("unmarshal episodic %q: %w", id, err)
		}
		// Skip consolidated records — they're already summaries.
		if len(rec.SourceIds) > 0 {
			return nil
		}

		ageSecs := 0.0
		if rec.Timestamp != nil {
			ageSecs = now.Sub(rec.Timestamp.AsTime()).Seconds()
			if ageSecs < 0 {
				ageSecs = 0
			}
		}
		recency := float32(math.Exp(-math.Ln2 * ageSecs / halfLifeSecs))

		// Importance defaults to 5 (see service.go), so scores never
		// collapse to zero purely from zero-importance. Access
		// frequency is a future extension — for now everyone starts
		// equal at 1.
		score := float32(rec.Importance) * recency

		out = append(out, scoredRecord{
			id:        id,
			record:    &rec,
			score:     score,
			retention: rec.Retention,
		})
		return nil
	})
	return out, err
}

// selectTopN picks the highest-scored records. Stable sort + slice;
// consolidation sees the same ordering regardless of bbolt iteration
// order.
func (d *DreamRunner) selectTopN(scored []scoredRecord, n int) []scoredRecord {
	if n <= 0 || len(scored) == 0 {
		return nil
	}
	sorted := make([]scoredRecord, len(scored))
	copy(sorted, scored)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].score > sorted[j].score })
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return sorted
}

// consolidate calls the Summarizer and writes the result as a new
// VectorRecord referencing the source IDs. The summarizer is allowed
// to return an empty summary — in that case we skip the write.
func (d *DreamRunner) consolidate(ctx context.Context, candidates []scoredRecord, now time.Time) error {
	events := make([]string, 0, len(candidates))
	sourceIDs := make([]string, 0, len(candidates))
	retentions := make([]lobslawv1.Retention, 0, len(candidates))
	for _, c := range candidates {
		events = append(events, c.record.Event)
		sourceIDs = append(sourceIDs, c.id)
		retentions = append(retentions, c.retention)
	}

	summary, embedding, err := d.summarizer.Summarize(ctx, events)
	if err != nil {
		return err
	}
	if summary == "" {
		return nil
	}

	// Consolidation inherits the highest retention tier among its
	// sources. If all sources were episodic, the consolidation is
	// episodic too — meaning a future dream run can prune it if its
	// own score falls below threshold. Only sources tagged long-term
	// make the consolidation long-term (durable summary).
	consolidated := &lobslawv1.VectorRecord{
		Id:        fmt.Sprintf("dream-%d", now.UnixNano()),
		Embedding: embedding,
		Text:      summary,
		Retention: highestRetention(retentions),
		SourceIds: sourceIDs,
		CreatedAt: timestamppb.New(now),
	}
	return d.applyEntry(&lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      consolidated.Id,
		Payload: &lobslawv1.LogEntry_VectorRecord{VectorRecord: consolidated},
	})
}

// highestRetention returns the most durable retention among sources.
// long-term > episodic > session > unspecified.
func highestRetention(rs []lobslawv1.Retention) lobslawv1.Retention {
	best := lobslawv1.Retention_RETENTION_UNSPECIFIED
	for _, r := range rs {
		// Enum values are ordered: UNSPECIFIED=0, SESSION=1,
		// EPISODIC=2, LONG_TERM=3 — higher = more durable.
		if r > best {
			best = r
		}
	}
	return best
}

// prune deletes records whose score is below the threshold, unless
// they carry long-term retention. Returns the count actually removed.
func (d *DreamRunner) prune(scored []scoredRecord) (int, error) {
	count := 0
	for _, r := range scored {
		if r.retention == lobslawv1.Retention_RETENTION_LONG_TERM {
			continue
		}
		if r.score >= d.cfg.PruneThreshold {
			continue
		}
		if err := d.applyEntry(&lobslawv1.LogEntry{
			Op:      lobslawv1.LogOp_LOG_OP_DELETE,
			Id:      r.id,
			Payload: &lobslawv1.LogEntry_EpisodicRecord{EpisodicRecord: &lobslawv1.EpisodicRecord{Id: r.id}},
		}); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// logDreamSession writes a tiny episodic record summarising what the
// dream pass did. Useful for post-hoc introspection ("what did the
// agent remember to forget last night?").
func (d *DreamRunner) logDreamSession(now time.Time, candidates, consolidated, pruned, digested, digests int) error {
	session := &lobslawv1.EpisodicRecord{
		Id:         fmt.Sprintf("dream-session-%d", now.UnixNano()),
		Event:      fmt.Sprintf("dream run: %d candidates, %d consolidated, %d pruned, %d commitments digested into %d rollups", candidates, consolidated, pruned, digested, digests),
		Importance: 3, // modest; dream sessions aren't the memories themselves
		Timestamp:  timestamppb.New(now),
		Tags:       []string{"dream-session"},
		Retention:  lobslawv1.Retention_RETENTION_LONG_TERM, // survive consolidation so audit trail persists
	}
	return d.applyEntry(&lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      session.Id,
		Payload: &lobslawv1.LogEntry_EpisodicRecord{EpisodicRecord: session},
	})
}

// digestCommitments rolls up fired commitments past the grace window
// into one EpisodicRecord per calendar day (UTC), then deletes the
// originals from BucketCommitments. Returns (deleted, digests, err).
//
// Digest IDs include UnixNano so a retry after a partial failure
// produces a fresh rollup rather than overwriting the prior one — the
// originals get deleted on the retry, and search recalls both summaries.
func (d *DreamRunner) digestCommitments(now time.Time) (int, int, error) {
	if d.cfg.CommitmentGrace <= 0 {
		return 0, 0, nil
	}
	cutoff := now.Add(-d.cfg.CommitmentGrace)

	// DueAt stands in for fire time — the proto has no fired_at field,
	// and the scheduler preserves DueAt when stamping Status=done.
	type entry struct {
		id     string
		due    time.Time
		reason string
		prompt string
	}
	byDay := map[string][]entry{}
	err := d.store.ForEach(BucketCommitments, func(_ string, raw []byte) error {
		var c lobslawv1.AgentCommitment
		if err := proto.Unmarshal(raw, &c); err != nil {
			return nil
		}
		if c.Status != "done" {
			return nil
		}
		if c.DueAt == nil {
			return nil
		}
		due := c.DueAt.AsTime()
		if !due.Before(cutoff) {
			return nil
		}
		dayKey := due.UTC().Format("2006-01-02")
		byDay[dayKey] = append(byDay[dayKey], entry{
			id:     c.Id,
			due:    due,
			reason: c.Reason,
			prompt: c.Params["prompt"],
		})
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("scan commitments: %w", err)
	}
	if len(byDay) == 0 {
		return 0, 0, nil
	}

	days := make([]string, 0, len(byDay))
	for k := range byDay {
		days = append(days, k)
	}
	sort.Strings(days)

	digested := 0
	digestsWritten := 0
	for _, day := range days {
		es := byDay[day]
		sort.Slice(es, func(i, j int) bool { return es[i].due.Before(es[j].due) })

		var lines strings.Builder
		fmt.Fprintf(&lines, "On %s (UTC), agent delivered %d scheduled commitment(s):\n", day, len(es))
		for _, e := range es {
			label := strings.TrimSpace(e.reason)
			if label == "" {
				label = truncatePrompt(e.prompt, 80)
			}
			if label == "" {
				label = "(unlabelled commitment)"
			}
			fmt.Fprintf(&lines, "- %s — %s\n", e.due.UTC().Format("15:04"), label)
		}

		digest := &lobslawv1.EpisodicRecord{
			Id:         fmt.Sprintf("commitment-digest-%s-%d", day, now.UnixNano()),
			Event:      fmt.Sprintf("commitment digest %s: %d delivered", day, len(es)),
			Context:    lines.String(),
			Importance: 4,
			Timestamp:  timestamppb.New(now),
			Tags:       []string{"commitment-history", "commitment-digest"},
			Retention:  lobslawv1.Retention_RETENTION_LONG_TERM,
		}
		if err := d.applyEntry(&lobslawv1.LogEntry{
			Op:      lobslawv1.LogOp_LOG_OP_PUT,
			Id:      digest.Id,
			Payload: &lobslawv1.LogEntry_EpisodicRecord{EpisodicRecord: digest},
		}); err != nil {
			return digested, digestsWritten, fmt.Errorf("write digest %s: %w", day, err)
		}
		digestsWritten++

		// Delete originals only after the digest write succeeds, so a
		// mid-pass crash leaves the originals intact and the next run
		// rebuilds the rollup.
		for _, e := range es {
			if err := d.applyEntry(&lobslawv1.LogEntry{
				Op:      lobslawv1.LogOp_LOG_OP_DELETE,
				Id:      e.id,
				Payload: &lobslawv1.LogEntry_Commitment{Commitment: &lobslawv1.AgentCommitment{Id: e.id}},
			}); err != nil {
				return digested, digestsWritten, fmt.Errorf("delete commitment %s: %w", e.id, err)
			}
			digested++
		}
	}
	return digested, digestsWritten, nil
}

// truncatePrompt clips a stored commitment prompt to a human-friendly
// length for digest summaries. Strips the leading "This commitment
// was scheduled by user in telegram chat ..." preamble that
// commitment_create injects, since the digest is always episodic and
// the chat-targeting metadata is noise in the summary.
func truncatePrompt(prompt string, max int) string {
	p := strings.TrimSpace(prompt)
	if idx := strings.Index(p, "Your task: "); idx >= 0 {
		p = strings.TrimSpace(p[idx+len("Your task: "):])
	}
	if max > 0 && len(p) > max {
		p = p[:max] + "…"
	}
	return p
}

func (d *DreamRunner) applyEntry(e *lobslawv1.LogEntry) error {
	if d.raft == nil {
		return fmt.Errorf("dream: raft stack not wired")
	}
	data, err := proto.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal log entry: %w", err)
	}
	resp, err := d.raft.Apply(data, applyTimeout)
	if err != nil {
		return fmt.Errorf("raft apply: %w", err)
	}
	if fsmErr, ok := resp.(error); ok && fsmErr != nil {
		return fmt.Errorf("fsm apply: %w", fsmErr)
	}
	return nil
}
