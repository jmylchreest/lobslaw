package memory

import (
	"fmt"
	"sort"
	"time"
)

// ForgetQuery selects the records a forget targets. The fields mirror
// lobslawv1.ForgetRequest's filters so the CLI and the RPC cannot
// diverge on what "forget these" means.
//
// IDs are taken at face value — the caller already decided. Text /
// Before / Tags feed the same scan the RPC uses.
type ForgetQuery struct {
	IDs    []string
	Text   string
	Before time.Time
	Tags   []string
}

// IsEmpty reports a query with no filter at all. An empty query
// matches every record in the store, so every caller has to decide
// deliberately whether to allow that; the RPC refuses it outright.
func (q ForgetQuery) IsEmpty() bool {
	return len(q.IDs) == 0 && q.Text == "" && q.Before.IsZero() && len(q.Tags) == 0
}

// hasScanFilter reports whether anything other than explicit IDs was
// supplied — i.e. whether the scan pass has work to do.
func (q ForgetQuery) hasScanFilter() bool {
	return q.Text != "" || !q.Before.IsZero() || len(q.Tags) > 0
}

// ForgetPlan is the resolved record set a forget would remove. Both
// ID slices are sorted so a dry-run prints the same thing twice.
type ForgetPlan struct {
	// Matched are the source records the query selected.
	Matched []string
	// Swept are the consolidations pulled down because at least one
	// of their SourceIds is in Matched. Keeping a summary whose
	// sources were deleted leaks the deleted content through the
	// summary's own text and embedding, so the cascade is the point
	// of forget rather than a side effect of it.
	Swept []string
	// Missing are explicitly-requested IDs that exist in neither
	// record bucket. Reported rather than silently dropped: for a
	// hand-typed id, "no such record" is nearly always a typo, and a
	// forget that quietly deletes nothing is the worst outcome.
	Missing []string
}

// Total is how many records the plan would delete.
func (p ForgetPlan) Total() int { return len(p.Matched) + len(p.Swept) }

// PlanForget resolves q into the exact set of records a forget would
// delete, without deleting anything. Split from ApplyForgetPlan so an
// offline caller can show the operator the blast radius first.
func PlanForget(store *Store, q ForgetQuery) (ForgetPlan, error) {
	return PlanForgetFor(store, q, Everyone())
}

// PlanForgetFor is PlanForget with a requester's read scope applied
// BETWEEN matching and cascading.
//
// The ordering is the point. Forget cascades through SourceIds and is
// irreversible, so a record the requester may not read has to leave
// the matched set before the cascade runs — otherwise it would pull
// its consolidations down with it, deleting through a record the
// caller was never allowed to see.
//
// Service.Forget used to carry its own copy of this matching, and the
// comment on ForgetQuery claimed the CLI and the RPC "cannot diverge
// on what forget these means" while two implementations sat either
// side of the wire. This is the one they now share.
func PlanForgetFor(store *Store, q ForgetQuery, audience Audience) (ForgetPlan, error) {
	matched := make(map[string]struct{}, len(q.IDs))
	var missing []string
	for _, id := range q.IDs {
		if id == "" {
			continue
		}
		ok, err := recordExists(store, id)
		if err != nil {
			return ForgetPlan{}, err
		}
		if !ok {
			missing = append(missing, id)
			continue
		}
		matched[id] = struct{}{}
	}

	if q.hasScanFilter() {
		scanned, err := forgetScan(store, q.Text, q.Before, q.Tags)
		if err != nil {
			return ForgetPlan{}, fmt.Errorf("forget scan: %w", err)
		}
		for id := range scanned {
			matched[id] = struct{}{}
		}
	}

	if err := retainForgettable(store, matched, audience); err != nil {
		return ForgetPlan{}, fmt.Errorf("forget scope: %w", err)
	}

	swept, err := forgetCascade(store, matched)
	if err != nil {
		return ForgetPlan{}, fmt.Errorf("forget cascade: %w", err)
	}
	// An explicitly-named consolidation is in both sets; count it once
	// so the dry-run totals add up.
	for id := range matched {
		delete(swept, id)
	}

	sort.Strings(missing)
	return ForgetPlan{
		Matched: sortedIDs(matched),
		Swept:   sortedIDs(swept),
		Missing: missing,
	}, nil
}

// ApplyForgetPlan deletes every record in the plan straight against
// the store, bypassing raft.
//
// Offline use only — the caller must hold the bbolt file lock, which
// means the node is stopped and there are no followers to diverge
// from. The live path goes through Service.Forget so the deletes are
// replicated.
func ApplyForgetPlan(store *Store, plan ForgetPlan) error {
	ids := make([]string, 0, plan.Total())
	ids = append(ids, plan.Matched...)
	ids = append(ids, plan.Swept...)
	for _, id := range ids {
		// The two record types share one id space and the plan does
		// not track which bucket each id came from. Delete is
		// idempotent, so hitting both is cheaper than looking it up.
		for _, bucket := range recordBuckets {
			if err := store.Delete(bucket, id); err != nil {
				return fmt.Errorf("delete %s/%s: %w", bucket, id, err)
			}
		}
	}
	return nil
}

// recordBuckets are the two buckets a memory record id can live in.
var recordBuckets = []string{BucketVectorRecords, BucketEpisodicRecords}

// recordExists reports whether id names a vector or episodic record.
func recordExists(store *Store, id string) (bool, error) {
	for _, bucket := range recordBuckets {
		_, err := store.Get(bucket, id)
		switch {
		case err == nil:
			return true, nil
		case IsNotFound(err):
			continue
		default:
			return false, fmt.Errorf("read %s/%s: %w", bucket, id, err)
		}
	}
	return false, nil
}

func sortedIDs(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
