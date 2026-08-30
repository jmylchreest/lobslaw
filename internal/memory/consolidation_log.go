package memory

import (
	"fmt"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Dream merges, supersedes and prunes memories on its own schedule.
// The verdict and the reason were computed on every run and then
// written to a log line and forgotten, so a user asking "why did it
// merge those two notes" had no answer, and one asking "what has it
// been doing to my memory" had none either.
//
// Memory that silently rewrites itself and cannot be inspected is a
// trust problem for a privacy-first product. This is the record.

// ConsolidationQuery filters a read of the log.
type ConsolidationQuery struct {
	// Owner, when set, restricts to one principal's memories. Empty
	// reads everything, which is for the offline CLI — a live caller
	// must scope, or the log describes one person's memories to
	// another.
	Owner string
	// Verdict, when set, restricts to one kind of decision.
	Verdict string
	// Since, when non-zero, drops anything older.
	Since time.Time
	// Limit caps the result. Zero is unlimited.
	Limit int
}

// ListConsolidations reads the log, newest first.
func ListConsolidations(store *Store, q ConsolidationQuery) ([]*lobslawv1.ConsolidationRecord, error) {
	var out []*lobslawv1.ConsolidationRecord
	err := store.ForEach(BucketConsolidations, func(_ string, raw []byte) error {
		var rec lobslawv1.ConsolidationRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return nil //nolint:nilerr // one unreadable entry should not hide the rest
		}
		if q.Owner != "" && rec.Owner != q.Owner {
			return nil
		}
		if q.Verdict != "" && rec.Verdict != q.Verdict {
			return nil
		}
		if !q.Since.IsZero() && rec.CreatedAt != nil && rec.CreatedAt.AsTime().Before(q.Since) {
			return nil
		}
		out = append(out, &rec)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read consolidation log: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		return consolidationTime(out[i]).After(consolidationTime(out[j]))
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func consolidationTime(r *lobslawv1.ConsolidationRecord) time.Time {
	if r.CreatedAt == nil {
		return time.Time{}
	}
	return r.CreatedAt.AsTime()
}
