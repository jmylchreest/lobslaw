package memory

import (
	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Reading back what Dream decided about a memory.
//
// The verdict lives once, in the consolidation log; BucketDisputes is
// the way from a record to the verdicts naming it. Recall asks this
// per hit, which is why it is an index lookup rather than a scan of
// the log — the log grows for the life of the node, and recall runs
// on every turn.

// DisputesFor returns the unresolved verdicts naming this memory,
// newest first. Empty for the overwhelming majority of records, which
// is the case worth being fast.
func DisputesFor(store *Store, episodicID string) ([]*lobslawv1.ConsolidationRecord, error) {
	if store == nil || episodicID == "" {
		return nil, nil
	}
	var out []*lobslawv1.ConsolidationRecord
	err := store.ForEachPrefix(BucketDisputes, episodicID+"/", func(_ string, value []byte) error {
		raw, err := store.Get(BucketConsolidations, string(value))
		if err != nil {
			// The verdict was pruned and the index outlived it. Not
			// an error: the record simply is not disputed any more.
			return nil //nolint:nilerr // a dangling index entry is absence, not failure
		}
		var rec lobslawv1.ConsolidationRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return nil //nolint:nilerr // one unreadable verdict must not hide the rest
		}
		out = append(out, &rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortConsolidationsNewestFirst(out)
	return out, nil
}

func sortConsolidationsNewestFirst(recs []*lobslawv1.ConsolidationRecord) {
	for i := 1; i < len(recs); i++ {
		for j := i; j > 0 && consolidationTime(recs[j]).After(consolidationTime(recs[j-1])); j-- {
			recs[j], recs[j-1] = recs[j-1], recs[j]
		}
	}
}

// CounterpartsOf is the other memories a verdict names. What the
// disputed record is being compared WITH — the half a reader needs
// and the previous design never surfaced.
func CounterpartsOf(rec *lobslawv1.ConsolidationRecord, episodicID string) []string {
	out := make([]string, 0, len(rec.GetSourceIds()))
	for _, sid := range rec.GetSourceIds() {
		if sid != "" && sid != episodicID {
			out = append(out, sid)
		}
	}
	return out
}
