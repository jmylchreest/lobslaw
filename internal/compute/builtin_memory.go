package compute

import (
	cryptorand "crypto/rand"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// memIDEntropy is the shared ULID entropy source for memory_write.
// Matches the pattern used in internal/audit — a single monotonic
// reader keeps IDs sortable within a millisecond; per-call fresh
// readers reset the counter and break monotonicity.
var memIDEntropy = ulid.Monotonic(cryptorand.Reader, 0)

// memoryRaftApplier is the subset of *memory.RaftNode the memory
// write tool needs. Interface so tests can substitute a fake.
type memoryRaftApplier interface {
	Apply(data []byte, timeout time.Duration) (any, error)
}

// MemoryConfig wires the memory_search + memory_write builtins.
// Both are registered together — reading without writing is a
// degraded state that confuses the model.
//
// Embedder, when non-nil, enables semantic vector search on
// memory_search. Without it, search falls back to substring match
// on episodic event/context fields. Auto-ingest (see
// internal/memory.EpisodicIngester) should use the same embedder
// so query vectors and stored vectors come from the same model.
type MemoryConfig struct {
	Store    *memory.Store
	Raft     memoryRaftApplier
	Embedder EmbeddingProvider
}

// RegisterMemoryBuiltins installs memory_search + memory_write
// when Store + Raft are supplied. Callers that don't want memory
// tooling simply don't call this; the tools won't appear in the
// LLM's function list.
func RegisterMemoryBuiltins(b *Builtins, cfg MemoryConfig) error {
	if cfg.Store == nil || cfg.Raft == nil {
		return errors.New("memory builtins: Store + Raft required")
	}
	if err := b.Register("memory_search", newMemorySearchHandler(cfg.Store, cfg.Embedder)); err != nil {
		return err
	}
	if err := b.Register("memory_write", newMemoryWriteHandler(cfg.Raft)); err != nil {
		return err
	}
	return nil
}

// MemoryToolDefs returns the ToolDef entries for both memory
// builtins. Kept with the registration helper so node.New iterates
// once.
func MemoryToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "memory_search",
			Path:        BuiltinScheme + "memory_search",
			Description: "Search stored memories for matches against a query. Use when the user references past conversations, preferences, facts they shared earlier, or decisions made. Returns matching records with event (summary), context (detail), tags, importance, and timestamp. Pass query as the keywords to match; optionally limit (default 5, max 20) and tag to filter by a specific tag.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "Keywords to match in event or context fields."},
					"limit": {"type": "integer", "description": "Max results (1-20). Default 5."},
					"tag": {"type": "string", "description": "Optional tag to filter results."}
				},
				"required": ["query"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "memory_write",
			Path:        BuiltinScheme + "memory_write",
			Description: "Commit a memory so future conversations can recall it. Use when the user shares a preference, fact about themselves, important decision, or something they explicitly ask you to remember. event is a short summary (one sentence); context is the full detail. Importance 1-10 (default 5). Tags help filtered recall later.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"event": {"type": "string", "description": "Short one-sentence summary."},
					"context": {"type": "string", "description": "Full detail text."},
					"importance": {"type": "integer", "description": "Score 1-10. Default 5."},
					"tags": {"type": "array", "items": {"type": "string"}, "description": "Optional tags."}
				},
				"required": ["event"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
	}
}

// newMemorySearchHandler prefers semantic vector search when an
// Embedder is configured, falls back to substring match over the
// EpisodicRecord fields. The fallback path is the original MVP
// behaviour — still useful for deployments without an embedding
// provider, and as a safety net when the embedder times out.
func newMemorySearchHandler(store *memory.Store, embedder EmbeddingProvider) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		query := strings.TrimSpace(args["query"])
		if query == "" {
			return nil, 2, errors.New("memory_search: query is required")
		}
		limit := 5
		if raw, ok := args["limit"]; ok && raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 20 {
				limit = n
			}
		}
		tagFilter := strings.TrimSpace(args["tag"])

		if embedder != nil {
			return runSemanticSearch(ctx, store, embedder, query, tagFilter, limit)
		}
		return runSubstringSearch(store, query, tagFilter, limit)
	}
}

// runSemanticSearch embeds the query, runs vectorSearch, then
// dereferences the source episodic records for the hits. If
// semantic returns fewer than `limit` hits, augments with
// substring matches — covers pre-embedding records that have
// no vector row yet. Returns fallback-substring on embedder
// failure.
func runSemanticSearch(ctx context.Context, store *memory.Store, embedder EmbeddingProvider, query, tagFilter string, limit int) ([]byte, int, error) {
	vec, err := embedder.Embed(ctx, query)
	if err != nil {
		payload, _, serr := runSubstringSearch(store, query, tagFilter, limit)
		return annotateEmbeddingFailure(payload, err), 0, serr
	}
	hits, err := memory.VectorSearch(store, vec, limit*2, "", "")
	if err != nil {
		payload, _, serr := runSubstringSearch(store, query, tagFilter, limit)
		return annotateEmbeddingFailure(payload, err), 0, serr
	}

	// Each VectorRecord carries source_ids pointing at episodic
	// records. Dereference them, apply tag filter, cap at limit.
	seen := map[string]bool{}
	results := make([]map[string]any, 0, limit)
	for _, h := range hits {
		for _, sid := range h.Record().SourceIds {
			if seen[sid] {
				continue
			}
			seen[sid] = true
			var epi lobslawv1.EpisodicRecord
			raw, err := store.Get(memory.BucketEpisodicRecords, sid)
			if err != nil {
				continue
			}
			if err := proto.Unmarshal(raw, &epi); err != nil {
				continue
			}
			if tagFilter != "" && !containsString(epi.Tags, tagFilter) {
				continue
			}
			results = append(results, episodicToMap(&epi, h.Score()))
			if len(results) >= limit {
				break
			}
		}
		if len(results) >= limit {
			break
		}
	}

	// Augment with substring matches when semantic under-
	// delivered. This is the common case during embedding
	// rollout: recent turns have vector records (found via
	// semantic), older turns don't (invisible without this
	// merge). Once the backfill runs, semantic dominates
	// naturally and this augmentation just no-ops.
	strategy := "semantic"
	if len(results) < limit {
		more := runSubstringMatches(store, query, tagFilter, limit-len(results), seen)
		if len(more) > 0 {
			results = append(results, more...)
			strategy = "semantic+substring"
		}
	}

	payload, err := json.Marshal(map[string]any{
		"query":    query,
		"results":  results,
		"strategy": strategy,
	})
	if err != nil {
		return nil, 1, err
	}
	return payload, 0, nil
}

// runSubstringMatches is the inner helper that returns the
// episodic-map results without the JSON envelope. Lets the
// semantic path augment its result set without round-tripping
// through JSON.
func runSubstringMatches(store *memory.Store, query, tagFilter string, limit int, exclude map[string]bool) []map[string]any {
	tokens := tokeniseQuery(query)
	if len(tokens) == 0 {
		return nil
	}
	type hit struct {
		rec   *lobslawv1.EpisodicRecord
		score int
	}
	var hits []hit
	_ = store.ForEach(memory.BucketEpisodicRecords, func(_ string, raw []byte) error {
		var r lobslawv1.EpisodicRecord
		if err := proto.Unmarshal(raw, &r); err != nil {
			return nil
		}
		if exclude[r.Id] {
			return nil
		}
		if tagFilter != "" && !containsString(r.Tags, tagFilter) {
			return nil
		}
		hay := strings.ToLower(r.Event + " " + r.Context)
		matches := 0
		for _, tok := range tokens {
			if strings.Contains(hay, tok) {
				matches++
			}
		}
		if matches == 0 {
			return nil
		}
		hits = append(hits, hit{rec: &r, score: matches})
		return nil
	})
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		if hits[i].rec.Importance != hits[j].rec.Importance {
			return hits[i].rec.Importance > hits[j].rec.Importance
		}
		return tsNano(hits[i].rec.Timestamp) > tsNano(hits[j].rec.Timestamp)
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, episodicToMap(h.rec, 0))
	}
	return out
}

// runSubstringSearch does tokenised BM25-ish lexical matching —
// NOT a single-substring match. Splits the query into words,
// drops noise (2-char and shorter), matches each word against the
// record's Event+Context lowercase. Score = number of distinct
// matching words weighted by importance. Rescues the common case
// where the user's phrasing doesn't literally contain the stored
// phrase — "where do I live" finds "User lives in Yorkshire" on
// the word "live" alone.
func runSubstringSearch(store *memory.Store, query, tagFilter string, limit int) ([]byte, int, error) {
	tokens := tokeniseQuery(query)
	type hit struct {
		rec   *lobslawv1.EpisodicRecord
		score int
	}
	var hits []hit
	err := store.ForEach(memory.BucketEpisodicRecords, func(_ string, raw []byte) error {
		var r lobslawv1.EpisodicRecord
		if err := proto.Unmarshal(raw, &r); err != nil {
			return nil
		}
		if tagFilter != "" && !containsString(r.Tags, tagFilter) {
			return nil
		}
		hay := strings.ToLower(r.Event + " " + r.Context)
		matches := 0
		for _, tok := range tokens {
			if strings.Contains(hay, tok) {
				matches++
			}
		}
		if matches == 0 {
			return nil
		}
		hits = append(hits, hit{rec: &r, score: matches})
		return nil
	})
	if err != nil {
		return nil, 1, fmt.Errorf("memory_search: scan: %w", err)
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		if hits[i].rec.Importance != hits[j].rec.Importance {
			return hits[i].rec.Importance > hits[j].rec.Importance
		}
		return tsNano(hits[i].rec.Timestamp) > tsNano(hits[j].rec.Timestamp)
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	results := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		results = append(results, episodicToMap(h.rec, 0))
	}
	payload, err := json.Marshal(map[string]any{
		"query":    query,
		"results":  results,
		"strategy": "tokenised-substring",
	})
	if err != nil {
		return nil, 1, err
	}
	return payload, 0, nil
}

// tokeniseQuery lowercases + splits on whitespace + drops
// stopwords and 1-2-char tokens. Preserves original word order
// (unused today but reserved for phrase-proximity scoring later).
func tokeniseQuery(query string) []string {
	fields := strings.Fields(strings.ToLower(query))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		// Strip trailing punctuation the user types casually.
		f = strings.Trim(f, ".,!?;:'\"()[]")
		if len(f) <= 2 {
			continue
		}
		if memorySearchStopwords[f] {
			continue
		}
		out = append(out, f)
	}
	return out
}

// memorySearchStopwords are low-signal words that generate too
// many hits to be useful. Conservative list — only the absolute
// worst offenders.
var memorySearchStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "but": true, "are": true,
	"was": true, "were": true, "has": true, "have": true, "had": true,
	"can": true, "you": true, "your": true, "this": true, "that": true,
	"what": true, "how": true, "why": true, "when": true, "where": true,
	"who": true, "which": true, "with": true, "from": true, "there": true,
	"then": true, "them": true, "they": true, "their": true, "will": true,
	"would": true, "could": true, "should": true, "about": true, "some": true,
	"all": true, "any": true, "not": true, "yes": true, "just": true,
}

func episodicToMap(rec *lobslawv1.EpisodicRecord, score float32) map[string]any {
	entry := map[string]any{
		"id":         rec.Id,
		"event":      rec.Event,
		"context":    rec.Context,
		"importance": rec.Importance,
		"tags":       rec.Tags,
	}
	if rec.Timestamp != nil {
		entry["timestamp"] = rec.Timestamp.AsTime().Format(time.RFC3339)
	}
	if score != 0 {
		entry["score"] = score
	}
	return entry
}

// annotateEmbeddingFailure adds a fallback-notice field to the
// substring payload so the operator can see in logs + model can
// surface to the user why recall might be less specific.
func annotateEmbeddingFailure(payload []byte, err error) []byte {
	var wrapped map[string]any
	if jerr := json.Unmarshal(payload, &wrapped); jerr != nil {
		return payload
	}
	wrapped["embedding_failed"] = err.Error()
	wrapped["strategy"] = "substring_fallback"
	out, merr := json.Marshal(wrapped)
	if merr != nil {
		return payload
	}
	return out
}

// newMemoryWriteHandler returns a BuiltinFunc that writes one
// EpisodicRecord via Raft. The ID is auto-generated (UUID) so the
// model doesn't need to synthesise one. Tags come in as a
// JSON-encoded string array from the LLM's tool-call arguments.
func newMemoryWriteHandler(raft memoryRaftApplier) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		event := strings.TrimSpace(args["event"])
		if event == "" {
			return nil, 2, errors.New("memory_write: event is required")
		}
		ctxField := args["context"]
		importance := int32(5)
		if raw, ok := args["importance"]; ok && raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 10 {
				importance = int32(n)
			}
		}
		var tags []string
		if raw, ok := args["tags"]; ok && raw != "" {
			if err := json.Unmarshal([]byte(raw), &tags); err != nil {
				return nil, 2, fmt.Errorf("tags must be a JSON array of strings: %w", err)
			}
		}

		id := ulid.MustNew(ulid.Now(), memIDEntropy).String()
		rec := &lobslawv1.EpisodicRecord{
			Id:         id,
			Event:      event,
			Context:    ctxField,
			Importance: importance,
			Tags:       tags,
			Timestamp:  timestamppb.Now(),
			Retention:  "long",
		}
		entry := &lobslawv1.LogEntry{
			Op: lobslawv1.LogOp_LOG_OP_PUT,
			Id: id,
			Payload: &lobslawv1.LogEntry_EpisodicRecord{
				EpisodicRecord: rec,
			},
		}
		data, err := proto.Marshal(entry)
		if err != nil {
			return nil, 1, fmt.Errorf("memory_write: marshal: %w", err)
		}
		res, err := raft.Apply(data, 5*time.Second)
		if err != nil {
			return nil, 1, fmt.Errorf("memory_write: raft apply: %w", err)
		}
		if fsmErr, ok := res.(error); ok && fsmErr != nil {
			return nil, 1, fmt.Errorf("memory_write: fsm: %w", fsmErr)
		}

		out, _ := json.Marshal(map[string]any{
			"id":         id,
			"event":      event,
			"importance": importance,
			"tags":       tags,
			"saved_at":   rec.Timestamp.AsTime().Format(time.RFC3339),
		})
		return out, 0, nil
	}
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func tsNano(ts *timestamppb.Timestamp) int64 {
	if ts == nil {
		return 0
	}
	return ts.AsTime().UnixNano()
}
