package memory

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// forgetScan walks VectorRecords and EpisodicRecords, returning the
// IDs that match the query. Separate from the delete step so the
// cascade pass can operate on a known-good source set.
//
// A record matches when ALL non-empty fields of the query match:
//
//	text substring   — against Text/Event/Context
//	before timestamp — record is older than before
//	tags             — at least one record tag overlaps with query.tags
//
// An empty query matches everything (nuclear option). Callers should
// validate at the gRPC layer.
func forgetScan(store *Store, query string, before time.Time, tags []string) (map[string]struct{}, error) {
	matchedSources := make(map[string]struct{})

	vectorMatch := func(v *lobslawv1.VectorRecord) bool {
		if query != "" && !strings.Contains(v.Text, query) {
			return false
		}
		if !before.IsZero() && v.CreatedAt != nil && !v.CreatedAt.AsTime().Before(before) {
			return false
		}
		if len(tags) > 0 {
			// VectorRecord doesn't carry tags natively — it stores
			// metadata. Accept a tag match against any metadata value.
			if !metadataMatchesAny(v.Metadata, tags) {
				return false
			}
		}
		return true
	}

	episodicMatch := func(e *lobslawv1.EpisodicRecord) bool {
		if query != "" && !(strings.Contains(e.Event, query) || strings.Contains(e.Context, query)) {
			return false
		}
		if !before.IsZero() && e.Timestamp != nil && !e.Timestamp.AsTime().Before(before) {
			return false
		}
		if len(tags) > 0 && !tagsOverlap(e.Tags, tags) {
			return false
		}
		return true
	}

	err := store.ForEach(BucketVectorRecords, func(id string, value []byte) error {
		var v lobslawv1.VectorRecord
		if err := proto.Unmarshal(value, &v); err != nil {
			return fmt.Errorf("unmarshal vector %q: %w", id, err)
		}
		// Skip consolidated records at the scan step — the cascade
		// step handles them separately so a consolidation doesn't
		// mask its own sources.
		if len(v.SourceIds) > 0 {
			return nil
		}
		if vectorMatch(&v) {
			matchedSources[id] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	err = store.ForEach(BucketEpisodicRecords, func(id string, value []byte) error {
		var e lobslawv1.EpisodicRecord
		if err := proto.Unmarshal(value, &e); err != nil {
			return fmt.Errorf("unmarshal episodic %q: %w", id, err)
		}
		if len(e.SourceIds) > 0 {
			// Consolidated episodic — handled in cascade step.
			return nil
		}
		if episodicMatch(&e) {
			matchedSources[id] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return matchedSources, nil
}

// forgetCascade returns the IDs of consolidated records whose sources
// intersect with matched. A consolidation with ANY source in matched
// is swept — MVP forget is aggressive by design, so a summary can't
// leak the content of a forgotten source through its remaining
// fragments.
func forgetCascade(store *Store, matched map[string]struct{}) (map[string]struct{}, error) {
	swept := make(map[string]struct{})

	check := func(id string, sources []string) {
		for _, src := range sources {
			if _, ok := matched[src]; ok {
				swept[id] = struct{}{}
				return
			}
		}
	}

	err := store.ForEach(BucketVectorRecords, func(id string, value []byte) error {
		var v lobslawv1.VectorRecord
		if err := proto.Unmarshal(value, &v); err != nil {
			return fmt.Errorf("unmarshal vector %q: %w", id, err)
		}
		if len(v.SourceIds) > 0 {
			check(id, v.SourceIds)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	err = store.ForEach(BucketEpisodicRecords, func(id string, value []byte) error {
		var e lobslawv1.EpisodicRecord
		if err := proto.Unmarshal(value, &e); err != nil {
			return fmt.Errorf("unmarshal episodic %q: %w", id, err)
		}
		if len(e.SourceIds) > 0 {
			check(id, e.SourceIds)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return swept, nil
}

func tagsOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, t := range a {
		set[t] = struct{}{}
	}
	for _, t := range b {
		if _, ok := set[t]; ok {
			return true
		}
	}
	return false
}

func metadataMatchesAny(md map[string]string, tags []string) bool {
	if len(md) == 0 || len(tags) == 0 {
		return false
	}
	for _, tag := range tags {
		for _, v := range md {
			if v == tag {
				return true
			}
		}
	}
	return false
}
