package memory

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Browsing the store, from either side of the wire.
//
// This scan used to live in cmd/lobslaw, where it could only run
// against a state.db on the same filesystem — which meant `memory
// list` on an operator's laptop listed nothing and said so as though
// the cluster were empty. Moving it here lets MemoryService answer the
// same question, with ONE definition of what each filter means.
//
// The alternative was a second scan on the service side, and two
// implementations of "--unowned" would drift the day somebody fixed a
// filter in one of them.

// Record kinds accepted by RecordFilter.Kind.
const (
	KindAll      = "all"
	KindVector   = "vector"
	KindEpisodic = "episodic"
)

// RecordFilter narrows a scan of the record buckets.
//
// Scope only exists on vector records and tags only on episodic ones,
// so either filter excludes the other kind OUTRIGHT rather than
// silently matching none of it — "--tag x" returning an unfiltered
// list of vectors would read as "these vectors carry that tag".
type RecordFilter struct {
	Kind    string
	Owner   string
	Scope   string
	Tag     string
	Unowned bool
	// Limit caps each kind separately. Zero means no cap. The totals in
	// RecordPage are the pre-limit counts, so a truncated listing can
	// say how much it is not showing.
	Limit int
}

// Validate normalises Kind and rejects an unknown one.
//
// Rejected rather than defaulted: a mistyped --kind that silently
// meant "all" would show an operator records they had asked to
// exclude.
func (f *RecordFilter) Validate() error {
	if f.Kind == "" {
		f.Kind = KindAll
	}
	switch f.Kind {
	case KindAll, KindVector, KindEpisodic:
		return nil
	}
	return fmt.Errorf("kind must be %s, %s or %s (got %q)",
		KindAll, KindVector, KindEpisodic, f.Kind)
}

func (f RecordFilter) keepVector(v *lobslawv1.VectorRecord) bool {
	if f.Tag != "" {
		return false
	}
	if f.Scope != "" && v.GetScope() != f.Scope {
		return false
	}
	return ownerMatches(v.GetOwner(), f.Owner, f.Unowned)
}

func (f RecordFilter) keepEpisodic(e *lobslawv1.EpisodicRecord) bool {
	if f.Scope != "" {
		return false
	}
	if f.Tag != "" && !slices.Contains(e.GetTags(), f.Tag) {
		return false
	}
	return ownerMatches(e.GetOwner(), f.Owner, f.Unowned)
}

// RecordPage is what matched, what survived the limit, and how much of
// it is anomalous.
type RecordPage struct {
	Vectors   []*lobslawv1.VectorRecord
	Episodics []*lobslawv1.EpisodicRecord
	// VectorTotal and EpisodicTotal are the counts BEFORE Limit, so a
	// caller can say "showing 20 of 400" rather than implying 20 is all
	// there is.
	VectorTotal   int
	EpisodicTotal int
	// Unowned counts records belonging to no principal. Ownership is
	// stamped on every record written since it existed, so an unowned
	// record today is a leftover or a write path that skipped the
	// field — either way it is attributable to nobody.
	Unowned int
}

// QueryRecords scans the record buckets, newest first.
func QueryRecords(store *Store, filter RecordFilter) (RecordPage, error) {
	if err := filter.Validate(); err != nil {
		return RecordPage{}, err
	}
	var page RecordPage

	if filter.Kind != KindEpisodic {
		err := store.ForEach(BucketVectorRecords, func(key string, raw []byte) error {
			var v lobslawv1.VectorRecord
			if err := proto.Unmarshal(raw, &v); err != nil {
				return fmt.Errorf("unmarshal vector %q: %w", key, err)
			}
			if !filter.keepVector(&v) {
				return nil
			}
			if v.GetOwner() == "" {
				page.Unowned++
			}
			page.Vectors = append(page.Vectors, &v)
			return nil
		})
		if err != nil {
			return RecordPage{}, err
		}
	}

	if filter.Kind != KindVector {
		err := store.ForEach(BucketEpisodicRecords, func(key string, raw []byte) error {
			var e lobslawv1.EpisodicRecord
			if err := proto.Unmarshal(raw, &e); err != nil {
				return fmt.Errorf("unmarshal episodic %q: %w", key, err)
			}
			if !filter.keepEpisodic(&e) {
				return nil
			}
			if e.GetOwner() == "" {
				page.Unowned++
			}
			page.Episodics = append(page.Episodics, &e)
			return nil
		})
		if err != nil {
			return RecordPage{}, err
		}
	}

	// Newest first: somebody scanning a store is nearly always looking
	// at what happened recently.
	sort.SliceStable(page.Vectors, func(i, j int) bool {
		return LaterThan(page.Vectors[i].GetCreatedAt(), page.Vectors[j].GetCreatedAt())
	})
	sort.SliceStable(page.Episodics, func(i, j int) bool {
		return LaterThan(page.Episodics[i].GetTimestamp(), page.Episodics[j].GetTimestamp())
	})

	page.VectorTotal, page.EpisodicTotal = len(page.Vectors), len(page.Episodics)
	if filter.Limit > 0 {
		if len(page.Vectors) > filter.Limit {
			page.Vectors = page.Vectors[:filter.Limit]
		}
		if len(page.Episodics) > filter.Limit {
			page.Episodics = page.Episodics[:filter.Limit]
		}
	}
	return page, nil
}

// FindRecord returns whichever record bucket holds id. Both nil with a
// nil error means it is in neither, which is not an error here — the
// caller decides whether a miss is one.
func FindRecord(store *Store, id string) (*lobslawv1.VectorRecord, *lobslawv1.EpisodicRecord, error) {
	raw, err := store.Get(BucketVectorRecords, id)
	switch {
	case err == nil:
		var v lobslawv1.VectorRecord
		if uerr := proto.Unmarshal(raw, &v); uerr != nil {
			return nil, nil, fmt.Errorf("unmarshal vector %q: %w", id, uerr)
		}
		return &v, nil, nil
	case !IsNotFound(err):
		return nil, nil, err
	}

	raw, err = store.Get(BucketEpisodicRecords, id)
	switch {
	case err == nil:
		var e lobslawv1.EpisodicRecord
		if uerr := proto.Unmarshal(raw, &e); uerr != nil {
			return nil, nil, fmt.Errorf("unmarshal episodic %q: %w", id, uerr)
		}
		return nil, &e, nil
	case !IsNotFound(err):
		return nil, nil, err
	}
	return nil, nil, nil
}

// ReferencedBy lists the consolidations naming id among their sources —
// exactly the set a forget would cascade into.
//
// Worth returning beside the record itself: it is what forgetting the
// record would take with it, and finding that out afterwards is too
// late.
func ReferencedBy(store *Store, id string) ([]string, error) {
	var out []string
	err := store.ForEach(BucketVectorRecords, func(key string, raw []byte) error {
		var v lobslawv1.VectorRecord
		if uerr := proto.Unmarshal(raw, &v); uerr != nil {
			return fmt.Errorf("unmarshal vector %q: %w", key, uerr)
		}
		if slices.Contains(v.GetSourceIds(), id) {
			out = append(out, v.GetId())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = store.ForEach(BucketEpisodicRecords, func(key string, raw []byte) error {
		var e lobslawv1.EpisodicRecord
		if uerr := proto.Unmarshal(raw, &e); uerr != nil {
			return fmt.Errorf("unmarshal episodic %q: %w", key, uerr)
		}
		if slices.Contains(e.GetSourceIds(), id) {
			out = append(out, e.GetId())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// ownerMatches applies the --owner / --unowned pair.
//
// The two are not the same question: --owner "" means "do not filter",
// while --unowned means "records attributable to nobody". Folding them
// into one flag would make the second unreachable.
func ownerMatches(owner, want string, unowned bool) bool {
	if unowned {
		return strings.TrimSpace(owner) == ""
	}
	if want == "" {
		return true
	}
	return owner == want
}

// LaterThan orders by timestamp, treating a missing one as oldest so
// records with no time sink rather than heading a newest-first list.
func LaterThan(a, b *timestamppb.Timestamp) bool {
	switch {
	case a == nil:
		return false
	case b == nil:
		return true
	}
	return a.AsTime().After(b.AsTime())
}
