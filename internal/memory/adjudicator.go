package memory

import (
	"context"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Deciding what a cluster of near-identical memories MEANS.
//
// Clustering is deterministic and says only that some records are
// worded alike. Whether "alike" means the same fact said twice, a
// fact that changed, or two facts that cannot both be true is a
// judgement, and cosine cannot make it.
//
// There is no default implementation on purpose. The last version of
// this shipped a stub that returned KeepDistinct unconditionally and
// was installed at construction, so the phase ran a similarity pass
// over long-term memory every night and threw away every verdict it
// produced. A nil Adjudicator now means the phase does not run at all
// — absence rather than a stub, so nothing pays for a decision nobody
// is making.

// MergeVerdict is what an Adjudicator concluded about one cluster.
type MergeVerdict string

const (
	// VerdictMerge: the same thing said more than once. Safe to
	// replace with a single record carrying all their sources.
	VerdictMerge MergeVerdict = "merge"

	// VerdictKeepDistinct: alike in wording, different in meaning.
	// Recorded rather than dropped, so the same cluster is not
	// re-adjudicated every night for the rest of its life.
	VerdictKeepDistinct MergeVerdict = "keep_distinct"

	// VerdictSupersedes: the same fact at different times, and one is
	// current. Never merged: what someone used to want is part of the
	// record of who they are.
	VerdictSupersedes MergeVerdict = "supersedes"

	// VerdictConflict: they cannot all be true and nothing in the
	// records says which is. NOT resolved automatically — see
	// dream_merge.go.
	VerdictConflict MergeVerdict = "conflict"
)

// Adjudication is one decision, with the reasoning that produced it.
//
// Reason is not decoration: it is what `lobslaw memory
// consolidations` shows a user asking why their memory changed, and
// the only thing standing between an automatic rewrite and an
// unaccountable one.
type Adjudication struct {
	Verdict MergeVerdict
	Reason  string

	// Consolidated is the single text that replaces the cluster.
	// Required for VerdictMerge and ignored otherwise; an empty one
	// downgrades the verdict to keep-distinct rather than deleting
	// records in favour of nothing.
	Consolidated string

	// Current is the record id that supersedes the rest. Required
	// for VerdictSupersedes; an id outside the cluster is refused,
	// because a verdict pointing at a record nobody clustered is one
	// nothing can act on.
	Current string
}

// Adjudicator decides what to do with a cluster of near-duplicates.
//
// Takes the cluster and returns a decision. It never writes: the
// caller owns every consequence, which is what keeps "what did it
// decide" and "what did it change" separable in the log.
type Adjudicator interface {
	AdjudicateMerge(ctx context.Context, cluster *lobslawv1.Cluster) (*Adjudication, error)
}
