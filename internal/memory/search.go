package memory

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// searchHit is an internal result from vector search before it's
// marshalled back to the gRPC response.
type searchHit struct {
	record *lobslawv1.VectorRecord
	score  float32
}

// Record exposes the underlying vector record for external callers
// (e.g. compute.memory_search → dereference to episodic).
func (h searchHit) Record() *lobslawv1.VectorRecord { return h.record }

// Score exposes the cosine similarity score.
func (h searchHit) Score() float32 { return h.score }

// VectorSearch runs a cosine search bounded by what the audience may
// read. The Audience is not optional and has no useful zero value: an
// unset one matches nothing, so a caller that forgets it gets empty
// results rather than everyone's memories. Callers that legitimately
// want the whole store say memory.Everyone().
func VectorSearch(store *Store, query []float32, limit int, audience Audience, scopeFilter string, retentionFilter lobslawv1.Retention) ([]searchHit, error) {
	return vectorSearch(store, query, limit, audience, scopeFilter, retentionFilter)
}

// scanUnmarshal decodes VectorRecord bytes into the narrow
// VectorScanEntry view, skipping text and metadata. Used by the
// wire-format test; the scan itself uses scanEntry.
var scanUnmarshal = proto.UnmarshalOptions{DiscardUnknown: true}

// Field numbers shared with VectorRecord, pinned by
// TestVectorScanEntryMatchesVectorRecordWireFormat.
const (
	fieldEmbedding  = 2
	fieldScope      = 5
	fieldRetention  = 6
	fieldOwner      = 9
	fieldVisibility = 10
	fieldNorm       = 11
	fieldSessionRef = 13
)

// scanEntry holds what the scan filters and scores on, reusing its
// embedding buffer across records. proto.Unmarshal cannot: Reset drops
// the slice rather than keeping capacity, costing D floats per record —
// 61 MB of the 130 MB a 10k-record query allocated at D=1536.
type scanEntry struct {
	embedding []float32
	scope     string
	owner     string
	// sessionRef is the conversation this record came from. Read on the
	// hot path because a conversation-scoped audience decides on it, and
	// deciding after the scan would mean filtering the top-N rather than
	// choosing it — the records that lose are the ones never scored.
	sessionRef string
	retention  lobslawv1.Retention
	visibility lobslawv1.Visibility
	norm       float32
}

func (e *scanEntry) decode(b []byte) error {
	e.embedding = e.embedding[:0]
	e.scope, e.owner, e.sessionRef = "", "", ""
	e.retention, e.visibility, e.norm = 0, 0, 0

	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]

		switch {
		case num == fieldEmbedding && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			for len(v) >= protobufFloat32Bytes {
				e.embedding = append(e.embedding, math.Float32frombits(binary.LittleEndian.Uint32(v)))
				v = v[protobufFloat32Bytes:]
			}
			b = b[n:]
		case num == fieldEmbedding && typ == protowire.Fixed32Type:
			// proto3 packs repeated floats, but unpacked is valid input.
			v, n := protowire.ConsumeFixed32(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			e.embedding = append(e.embedding, math.Float32frombits(v))
			b = b[n:]
		case num == fieldScope && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			e.scope = string(v)
			b = b[n:]
		case num == fieldOwner && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			e.owner = string(v)
			b = b[n:]
		case num == fieldRetention && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			e.retention = lobslawv1.Retention(v)
			b = b[n:]
		case num == fieldVisibility && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			e.visibility = lobslawv1.Visibility(v)
			b = b[n:]
		case num == fieldNorm && typ == protowire.Fixed32Type:
			v, n := protowire.ConsumeFixed32(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			e.norm = math.Float32frombits(v)
			b = b[n:]
		case num == fieldSessionRef && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			e.sessionRef = string(v)
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	return nil
}

// vectorSearch returns the top-K records by cosine similarity.
//
// Two phases: scan every record decoding only what filters and scoring
// need, keeping a bounded top-K of ids; then load the full records for
// the K survivors. Still O(N × D) — a prefilter that avoids visiting
// every record is separate work — but only K records decode their text.
func vectorSearch(store *Store, query []float32, limit int, audience Audience, scopeFilter string, retentionFilter lobslawv1.Retention) ([]searchHit, error) {
	if len(query) == 0 {
		return nil, errors.New("search query embedding is empty")
	}
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	queryNorm := norm(query)
	if queryNorm == 0 {
		return nil, errors.New("search query embedding has zero norm")
	}

	top := newTopK(limit)
	// Mixed embedding widths are valid, but skipping the whole corpus
	// looks identical to having no memories — so count and warn.
	var dimMismatch int
	var entry scanEntry
	err := store.ForEach(BucketVectorRecords, func(key string, value []byte) error {
		if err := entry.decode(value); err != nil {
			return fmt.Errorf("decode vector record %s: %w", key, err)
		}
		// Ownership before anything else: a record this audience may
		// not read should not reach scoring, let alone a result set.
		if !audience.allows(entry.owner, entry.visibility, entry.sessionRef) {
			return nil
		}
		if scopeFilter != "" && entry.scope != scopeFilter {
			return nil
		}
		if retentionFilter != lobslawv1.Retention_RETENTION_UNSPECIFIED && entry.retention != retentionFilter {
			return nil
		}
		if len(entry.embedding) != len(query) {
			dimMismatch++
			return nil
		}
		// Zero norm means a zero vector or a record predating the field.
		candNorm := entry.norm
		if candNorm == 0 {
			candNorm = norm(entry.embedding)
			if candNorm == 0 {
				return nil
			}
		}
		top.push(key, dot(query, entry.embedding)/(queryNorm*candNorm))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if dimMismatch > 0 {
		// slog.Default because this is a free function; threading the
		// node logger through is a follow-up.
		slog.Default().Warn("memory: vector search skipped records with mismatched embedding width",
			"skipped", dimMismatch, "query_dim", len(query))
	}

	return hydrate(store, top.sorted())
}

// hydrate loads the full record for each survivor, in score order. A
// candidate deleted between scan and load is skipped, not an error: the
// scan holds no lock, so a concurrent Forget or Dream merge is expected.
func hydrate(store *Store, cands []candidate) ([]searchHit, error) {
	hits := make([]searchHit, 0, len(cands))
	for _, c := range cands {
		raw, err := store.Get(BucketVectorRecords, c.key)
		if err != nil {
			if errors.Is(err, types.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("load vector record %s: %w", c.key, err)
		}
		var rec lobslawv1.VectorRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("unmarshal vector record %s: %w", c.key, err)
		}
		hits = append(hits, searchHit{record: &rec, score: c.score})
	}
	return hits, nil
}

type candidate struct {
	key   string
	score float32
}

// topK keeps the best `limit` candidates in a min-heap, so the weakest
// survivor sits at index 0 and evicts in O(log K). Replaces
// append-everything-then-sort, which cost O(N) memory for a top-3
// result. heap.Interface is skipped: at K of 3–10 the dispatch costs
// more than the sift.
type topK struct {
	limit int
	items []candidate
}

func newTopK(limit int) *topK {
	return &topK{limit: limit, items: make([]candidate, 0, limit)}
}

func (t *topK) push(key string, score float32) {
	if len(t.items) < t.limit {
		t.items = append(t.items, candidate{key: key, score: score})
		t.up(len(t.items) - 1)
		return
	}
	// NaN fails this test, so it is dropped rather than poisoning order.
	if !(score > t.items[0].score) {
		return
	}
	t.items[0] = candidate{key: key, score: score}
	t.down(0)
}

func (t *topK) up(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if t.items[parent].score <= t.items[i].score {
			return
		}
		t.items[parent], t.items[i] = t.items[i], t.items[parent]
		i = parent
	}
}

func (t *topK) down(i int) {
	n := len(t.items)
	for {
		smallest := i
		for _, child := range [2]int{2*i + 1, 2*i + 2} {
			if child < n && t.items[child].score < t.items[smallest].score {
				smallest = child
			}
		}
		if smallest == i {
			return
		}
		t.items[i], t.items[smallest] = t.items[smallest], t.items[i]
		i = smallest
	}
}

// sorted returns descending score, ties broken on key so unchanged data
// gives a stable order — reshuffling recall invalidates the model's
// prompt prefix cache.
func (t *topK) sorted() []candidate {
	out := t.items
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].key < out[j].key
	})
	return out
}

// dot computes the dot product of two equal-length float32 slices.
// Assumes len(a) == len(b) — caller is responsible.
func dot(a, b []float32) float32 {
	var sum float32
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}

// norm returns the L2 norm of v. Returns 0 for an empty or zero vector.
func norm(v []float32) float32 {
	var sum float32
	for _, x := range v {
		sum += x * x
	}
	return float32(math.Sqrt(float64(sum)))
}
