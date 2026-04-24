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

// memoryForgetter is the subset of *memory.Service needed for
// memory_forget. Interface so tests can substitute a fake.
type memoryForgetter interface {
	Forget(ctx context.Context, req *lobslawv1.ForgetRequest) (*lobslawv1.ForgetResponse, error)
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
	Store     *memory.Store
	Raft      memoryRaftApplier
	Forgetter memoryForgetter // enables memory_forget + memory_correct; nil → those builtins skip registration
	Embedder  EmbeddingProvider
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
	if err := b.Register("memory_recent", newMemoryRecentHandler(cfg.Store)); err != nil {
		return err
	}
	if err := b.Register("dream_recap", newDreamRecapHandler(cfg.Store)); err != nil {
		return err
	}
	if cfg.Forgetter != nil {
		if err := b.Register("memory_forget", newMemoryForgetHandler(cfg.Forgetter)); err != nil {
			return err
		}
		if err := b.Register("memory_correct", newMemoryCorrectHandler(cfg.Raft, cfg.Forgetter)); err != nil {
			return err
		}
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
		{
			Name:        "memory_recent",
			Path:        BuiltinScheme + "memory_recent",
			Description: "List memories written recently. Use when the user asks 'what have you learned about me recently' or 'what's new in memory'. Optionally filter by retention (session|episodic|long-term) and a cutoff duration (since). Returns up to limit entries (default 20) sorted newest-first. Present as a markdown table or bullet list — this is fact-dense enumerable content, not narrative.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"retention": {"type": "string", "description": "Filter by retention tier: session | episodic | long-term. Default: all."},
					"since": {"type": "string", "description": "Only include entries newer than this duration ago (e.g. '24h', '7d'). Default: no filter."},
					"limit": {"type": "integer", "description": "Max entries (1-50). Default 20."}
				},
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "dream_recap",
			Path:        BuiltinScheme + "dream_recap",
			Description: "Show what was consolidated during recent REM/dream cycles. Returns vector records tagged as consolidations with their source_id counts, consolidation timestamps, and summary text. Use when the user asks 'what did you dream about', 'what did you consolidate', or 'what did you learn last night'. Narrate the result in your own voice per Personality & Style — don't dump the raw structure. Optional since filter (e.g. '24h', '7d', default all-time).",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"since": {"type": "string", "description": "Only include consolidations newer than this (e.g. '24h'). Default: all."},
					"limit": {"type": "integer", "description": "Max entries (1-50). Default 10."}
				},
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "memory_forget",
			Path:        BuiltinScheme + "memory_forget",
			Description: "Delete memories matching the given filter. Cascades: any consolidated memory whose source_ids intersect the matched set is ALSO deleted (privacy-safe: won't leave summaries echoing forgotten content). DESTRUCTIVE — requires confirmation. Pass at least one filter: query (substring match), ids (explicit list), before (RFC3339 cutoff), or tags. Returns count deleted.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "Substring match against event or context."},
					"ids": {"type": "array", "items": {"type": "string"}, "description": "Explicit list of memory IDs to delete."},
					"before": {"type": "string", "description": "Delete entries older than this RFC3339 timestamp (e.g. 2026-04-01T00:00:00Z)."},
					"tags": {"type": "array", "items": {"type": "string"}, "description": "Match entries carrying any of these tags."}
				},
				"additionalProperties": false
			}`),
			RiskTier: types.RiskIrreversible,
		},
		{
			Name:        "memory_correct",
			Path:        BuiltinScheme + "memory_correct",
			Description: "Supersede an existing memory with corrected content. Writes a new memory with updated text, then forgets the original — audit log preserves the change. Use when you realise a stored fact is wrong (user said 'I moved last week, your memory still says Y'). No confirmation required (it's improving, not destroying). Pass id of the memory to supersede plus new_event (one-sentence summary) and optionally new_context (full detail).",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"id": {"type": "string", "description": "ID of the memory to supersede."},
					"new_event": {"type": "string", "description": "Updated one-sentence summary."},
					"new_context": {"type": "string", "description": "Updated full detail (optional)."}
				},
				"required": ["id", "new_event"],
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

// newDreamRecapHandler lists vector records whose SourceIds count
// is > 1 (indicating they are consolidations produced by a dream/REM
// cycle), newest-first. Read-only; safe on followers. Narration
// discipline is enforced prompt-side — the tool returns structured
// JSON; the bot re-renders in voice.
func newDreamRecapHandler(store *memory.Store) BuiltinFunc {
	return func(_ context.Context, args map[string]string) ([]byte, int, error) {
		limit := 10
		if raw, ok := args["limit"]; ok && raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 50 {
				limit = n
			}
		}
		var cutoff time.Time
		if raw, ok := args["since"]; ok && raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return nil, 2, fmt.Errorf("dream_recap: since must be a duration like '24h': %w", err)
			}
			cutoff = time.Now().Add(-d)
		}
		type recap struct {
			ID             string   `json:"id"`
			Text           string   `json:"text"`
			Scope          string   `json:"scope,omitempty"`
			SourceCount    int      `json:"source_count"`
			SourceIDs      []string `json:"source_ids"`
			ConsolidatedAt string   `json:"consolidated_at"`
			unix           int64
		}
		var all []recap
		err := store.ForEach(memory.BucketVectorRecords, func(_ string, raw []byte) error {
			var v lobslawv1.VectorRecord
			if err := proto.Unmarshal(raw, &v); err != nil {
				return nil
			}
			if len(v.SourceIds) < 2 {
				return nil
			}
			t := v.CreatedAt.AsTime()
			if !cutoff.IsZero() && t.Before(cutoff) {
				return nil
			}
			all = append(all, recap{
				ID:             v.Id,
				Text:           v.Text,
				Scope:          v.Scope,
				SourceCount:    len(v.SourceIds),
				SourceIDs:      v.SourceIds,
				ConsolidatedAt: t.Format(time.RFC3339),
				unix:           t.UnixNano(),
			})
			return nil
		})
		if err != nil {
			return nil, 1, fmt.Errorf("dream_recap: scan: %w", err)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].unix > all[j].unix })
		if len(all) > limit {
			all = all[:limit]
		}
		out, err := json.Marshal(map[string]any{
			"count":          len(all),
			"consolidations": all,
		})
		if err != nil {
			return nil, 1, err
		}
		return out, 0, nil
	}
}

// newMemoryForgetHandler wraps memory.Service.Forget with the
// builtin JSON arg shape. Requires raft leader (Service.Forget
// errors otherwise); confirmation is enforced by the policy layer
// via RiskIrreversible.
func newMemoryForgetHandler(svc memoryForgetter) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		query := strings.TrimSpace(args["query"])
		var ids []string
		if raw, ok := args["ids"]; ok && raw != "" {
			if err := json.Unmarshal([]byte(raw), &ids); err != nil {
				return nil, 2, fmt.Errorf("memory_forget: ids must be a JSON array: %w", err)
			}
		}
		var tags []string
		if raw, ok := args["tags"]; ok && raw != "" {
			if err := json.Unmarshal([]byte(raw), &tags); err != nil {
				return nil, 2, fmt.Errorf("memory_forget: tags must be a JSON array: %w", err)
			}
		}
		var before *timestamppb.Timestamp
		if raw, ok := args["before"]; ok && raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return nil, 2, fmt.Errorf("memory_forget: before must be RFC3339: %w", err)
			}
			before = timestamppb.New(t)
		}
		if query == "" && len(ids) == 0 && len(tags) == 0 && before == nil {
			return nil, 2, errors.New("memory_forget: at least one filter required (query, ids, tags, or before) — refusing to forget everything")
		}
		req := &lobslawv1.ForgetRequest{
			Query:  query,
			Ids:    ids,
			Tags:   tags,
			Before: before,
		}
		resp, err := svc.Forget(ctx, req)
		if err != nil {
			return nil, 1, fmt.Errorf("memory_forget: %w", err)
		}
		out, err := json.Marshal(map[string]any{
			"deleted_count":        resp.RecordsRemoved,
			"consolidations_swept": resp.ConsolidationsReforged,
		})
		if err != nil {
			return nil, 1, err
		}
		return out, 0, nil
	}
}

// newMemoryCorrectHandler writes a new memory with superseded
// metadata, then forgets the original by id. Two-step operation
// but single transactional intent: audit log retains both the new
// write and the forget, preserving the correction trail.
func newMemoryCorrectHandler(raft memoryRaftApplier, forgetter memoryForgetter) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		oldID := strings.TrimSpace(args["id"])
		if oldID == "" {
			return nil, 2, errors.New("memory_correct: id is required")
		}
		newEvent := strings.TrimSpace(args["new_event"])
		if newEvent == "" {
			return nil, 2, errors.New("memory_correct: new_event is required")
		}
		newContext := args["new_context"]

		// Step 1: write the correction as a new memory with a
		// "corrects:<old_id>" tag so the audit trail is queryable.
		newID := ulid.MustNew(ulid.Now(), memIDEntropy).String()
		newRec := &lobslawv1.EpisodicRecord{
			Id:         newID,
			Event:      newEvent,
			Context:    newContext,
			Importance: 5,
			Tags:       []string{"corrects:" + oldID},
			Timestamp:  timestamppb.Now(),
			Retention:  "long",
		}
		entry := &lobslawv1.LogEntry{
			Op: lobslawv1.LogOp_LOG_OP_PUT,
			Id: newID,
			Payload: &lobslawv1.LogEntry_EpisodicRecord{
				EpisodicRecord: newRec,
			},
		}
		data, err := proto.Marshal(entry)
		if err != nil {
			return nil, 1, fmt.Errorf("memory_correct: marshal: %w", err)
		}
		if _, err := raft.Apply(data, 5*time.Second); err != nil {
			return nil, 1, fmt.Errorf("memory_correct: raft apply new: %w", err)
		}

		// Step 2: forget the original. Any consolidations containing
		// the old id are also swept (privacy-safe).
		forgetReq := &lobslawv1.ForgetRequest{Ids: []string{oldID}}
		forgetResp, err := forgetter.Forget(ctx, forgetReq)
		if err != nil {
			return nil, 1, fmt.Errorf("memory_correct: forget old: %w", err)
		}

		out, err := json.Marshal(map[string]any{
			"new_id":               newID,
			"old_id":               oldID,
			"deleted_count":        forgetResp.RecordsRemoved,
			"consolidations_swept": forgetResp.ConsolidationsReforged,
		})
		if err != nil {
			return nil, 1, err
		}
		return out, 0, nil
	}
}

// newMemoryRecentHandler lists recent episodic memory writes sorted
// newest-first, with optional retention and since-duration filters.
// Read-only: no Raft proposal, safe on followers. Returns fact-dense
// enumerable JSON — the agent is instructed (via humanisation rule)
// to render this as a table/bullets, not narration.
func newMemoryRecentHandler(store *memory.Store) BuiltinFunc {
	return func(_ context.Context, args map[string]string) ([]byte, int, error) {
		limit := 20
		if raw, ok := args["limit"]; ok && raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 50 {
				limit = n
			}
		}
		retentionFilter := strings.TrimSpace(strings.ToLower(args["retention"]))

		var cutoff time.Time
		if raw, ok := args["since"]; ok && raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return nil, 2, fmt.Errorf("memory_recent: since must be a duration like '24h' or '7d': %w", err)
			}
			cutoff = time.Now().Add(-d)
		}

		type entry struct {
			ID         string   `json:"id"`
			Event      string   `json:"event"`
			Context    string   `json:"context,omitempty"`
			Retention  string   `json:"retention"`
			Importance int32    `json:"importance"`
			Tags       []string `json:"tags,omitempty"`
			Timestamp  string   `json:"timestamp"`
			unix       int64
		}
		var all []entry

		err := store.ForEach(memory.BucketEpisodicRecords, func(_ string, raw []byte) error {
			var rec lobslawv1.EpisodicRecord
			if err := proto.Unmarshal(raw, &rec); err != nil {
				return nil
			}
			if retentionFilter != "" && !strings.EqualFold(rec.Retention, retentionFilter) {
				return nil
			}
			t := rec.Timestamp.AsTime()
			if !cutoff.IsZero() && t.Before(cutoff) {
				return nil
			}
			all = append(all, entry{
				ID:         rec.Id,
				Event:      rec.Event,
				Context:    rec.Context,
				Retention:  rec.Retention,
				Importance: rec.Importance,
				Tags:       rec.Tags,
				Timestamp:  t.Format(time.RFC3339),
				unix:       t.UnixNano(),
			})
			return nil
		})
		if err != nil {
			return nil, 1, fmt.Errorf("memory_recent: scan: %w", err)
		}

		sort.Slice(all, func(i, j int) bool { return all[i].unix > all[j].unix })
		if len(all) > limit {
			all = all[:limit]
		}

		out, err := json.Marshal(map[string]any{
			"count":     len(all),
			"retention": retentionFilter,
			"entries":   all,
		})
		if err != nil {
			return nil, 1, err
		}
		return out, 0, nil
	}
}
