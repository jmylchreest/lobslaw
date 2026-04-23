package memory

import (
	"context"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// MergeVerdict is the LLM's judgement on what to do with a cluster
// of near-duplicate records. The four options distinguish actions
// that lose information (Merge) from actions that preserve it
// (KeepDistinct / Conflict / Supersedes).
//
// KeepDistinct is the safe default — chosen when the Adjudicator
// is uncertain, when the LLM call fails, or when we simply haven't
// wired up an LLM yet (the stub returns this unconditionally).
// False-merge is irreversible; false-no-merge is just bloat.
type MergeVerdict int

const (
	// MergeVerdictKeepDistinct — leave the records alone. Same topic
	// but genuinely different (e.g. "asked about cheese" vs "told me
	// cheese is good"). The zero value; any failure path returns this.
	MergeVerdictKeepDistinct MergeVerdict = iota

	// MergeVerdictMerge — records express the same fact in different
	// words. Create one consolidated record from MergedText, delete
	// the originals via Forget(ids).
	MergeVerdictMerge

	// MergeVerdictConflict — records contradict each other (e.g. "likes
	// cheese" vs "hates cheese"). Preserve all; tag cluster so future
	// Recalls surface the disagreement.
	MergeVerdictConflict

	// MergeVerdictSupersedes — newer record updates an older fact
	// (e.g. dated "liked cheese in 2023" and "stopped eating cheese
	// in 2026"). Tag as a supersession chain; keep both.
	MergeVerdictSupersedes
)

// String returns the canonical wire/audit spelling of the verdict.
// Used in audit log entries and in test assertions.
func (v MergeVerdict) String() string {
	switch v {
	case MergeVerdictKeepDistinct:
		return "keep_distinct"
	case MergeVerdictMerge:
		return "merge"
	case MergeVerdictConflict:
		return "conflict"
	case MergeVerdictSupersedes:
		return "supersedes"
	default:
		return "unknown"
	}
}

// MergeDecision is the Adjudicator's full output: what to do, why,
// and (for Merge) the canonical consolidated text.
type MergeDecision struct {
	// Verdict is the action to take. See MergeVerdict.
	Verdict MergeVerdict

	// MergedText is the consolidated form when Verdict == Merge.
	// Ignored otherwise. Callers write this into the new VectorRecord
	// created for the consolidation.
	MergedText string

	// Reason is a short free-form explanation of the verdict. Always
	// populated so the audit log tells operators *why* a cluster was
	// left distinct / merged / tagged-conflict.
	Reason string
}

// Adjudicator renders merge/keep-distinct/conflict/supersedes
// judgements on clusters of near-duplicate records. Deliberately
// NOT part of the memory service — the interface lives here so the
// Dream runner can inject whichever implementation suits its
// deployment (cheap-fast model vs smart-expensive; remote LLM vs
// local). Phase 5 provides the first real LLM-backed implementation.
//
// Implementations must be safe to call from multiple goroutines;
// Dream doesn't currently parallelise cluster adjudication but the
// contract keeps that future open.
//
// On error, callers must treat the cluster as KeepDistinct — never
// take a destructive action on a failed LLM call.
type Adjudicator interface {
	AdjudicateMerge(ctx context.Context, cluster *lobslawv1.Cluster) (MergeDecision, error)
}

// AlwaysKeepDistinctAdjudicator is the zero-cost, zero-risk default
// implementation that returns KeepDistinct for every cluster. Used
// as:
//
//   - The default when the operator hasn't configured an LLM yet
//     (boot-safe: no merges happen, no data lost).
//   - The standard test double — unit tests exercising the merge
//     flow's plumbing don't need a real LLM.
//
// Phase 5 supersedes this with a real LLM-backed Adjudicator.
type AlwaysKeepDistinctAdjudicator struct{}

// AdjudicateMerge satisfies the Adjudicator interface. Always
// returns KeepDistinct with an explanatory Reason so audit logs
// don't look like empty judgements.
func (AlwaysKeepDistinctAdjudicator) AdjudicateMerge(_ context.Context, _ *lobslawv1.Cluster) (MergeDecision, error) {
	return MergeDecision{
		Verdict: MergeVerdictKeepDistinct,
		Reason:  "no LLM Adjudicator configured (stub: always KeepDistinct)",
	}, nil
}
