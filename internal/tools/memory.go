package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/promptguard"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

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

// memoryDreamer is the subset of *memory.Service needed for
// dream_nap (on-demand consolidation). Interface so tests can
// substitute a fake.
type memoryDreamer interface {
	Dream(ctx context.Context, req *lobslawv1.DreamRequest) (*lobslawv1.DreamResponse, error)
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
	Dreamer   memoryDreamer   // enables dream_nap; nil → builtin skips registration
	Embedder  compute.EmbeddingProvider

	// CrossOwner decides whether a turn may read or forget records
	// owned by someone else. Nil never widens, so a deployment that
	// has not wired it behaves exactly as it did before the operator
	// role existed.
	CrossOwner compute.CrossOwnerAuthorizer
}

// RegisterMemoryBuiltins installs memory_search + memory_write
// when Store + Raft are supplied. Callers that don't want memory
// tooling simply don't call this; the tools won't appear in the
// LLM's function list.
func RegisterMemoryBuiltins(b *Builtins, cfg MemoryConfig) error {
	if cfg.Store == nil || cfg.Raft == nil {
		return errors.New("memory builtins: Store + Raft required")
	}
	if err := b.Register("memory_search", newMemorySearchHandler(cfg.Store, cfg.Embedder, cfg.CrossOwner)); err != nil {
		return err
	}
	if err := b.Register("memory_write", newMemoryWriteHandler(cfg.Raft, cfg.Embedder)); err != nil {
		return err
	}
	if err := b.Register("memory_recent", newMemoryRecentHandler(cfg.Store, cfg.CrossOwner)); err != nil {
		return err
	}
	if err := b.Register("dream_recap", newDreamRecapHandler(cfg.Store, cfg.CrossOwner)); err != nil {
		return err
	}
	if cfg.Forgetter != nil {
		if err := b.Register("memory_forget", newMemoryForgetHandler(cfg.Forgetter, cfg.CrossOwner)); err != nil {
			return err
		}
		if err := b.Register("memory_correct", newMemoryCorrectHandler(cfg.Store, cfg.Raft, cfg.Forgetter, cfg.CrossOwner, cfg.Embedder)); err != nil {
			return err
		}
	}
	if cfg.Dreamer != nil {
		if err := b.Register("dream_nap", newDreamNapHandler(cfg.Dreamer)); err != nil {
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
			Path:        compute.BuiltinScheme + "memory_search",
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
			Path:        compute.BuiltinScheme + "memory_write",
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
			Path:        compute.BuiltinScheme + "memory_recent",
			Description: "List memories written recently. Use when the user asks 'what have you learned about me recently' or 'what's new in memory'. Optionally filter by retention (session|episodic|long-term) and a cutoff duration (since). Returns up to limit entries (default 20) sorted newest-first. Present as a markdown table or bullet list — this is fact-dense enumerable content, not narrative.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"retention": {"type": "string", "description": "Filter by retention Tier: session | episodic | long-term. Default: all."},
					"since": {"type": "string", "description": "Only include entries newer than this duration ago (e.g. '24h', '7d'). Default: no filter."},
					"limit": {"type": "integer", "description": "Max entries (1-50). Default 20."}
				},
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "dream_recap",
			Path:        compute.BuiltinScheme + "dream_recap",
			Description: "Show what was consolidated during recent REM/dream cycles. Returns vector records tagged as consolidations with their source_id counts, consolidation timestamps, and summary text. Use when the user asks 'what did you dream about', 'what did you consolidate', or 'what did you learn last night'. Always narrate the result in your own voice per Personality & Style — summarise what you learned, omit the raw structure. Optional since filter (e.g. '24h', '7d', default all-time).",
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
			Path:        compute.BuiltinScheme + "memory_forget",
			Description: "Delete memories matching the given filter. Cascades: any consolidated memory whose source_ids intersect the matched set is also deleted (privacy-safe — summaries that echo forgotten content go too). DESTRUCTIVE — requires confirmation. Pass at least one filter: query (substring match), ids (explicit list), before (RFC3339 cutoff), or tags. Returns count deleted.",
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
			Path:        compute.BuiltinScheme + "memory_correct",
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
		{
			Name:        "dream_nap",
			Path:        compute.BuiltinScheme + "dream_nap",
			Description: "Trigger an on-demand Dream/REM consolidation pass right now (the 'nap' before the scheduled nightly dream). Scores recent memories, consolidates clusters into summaries when a Summarizer is wired, and prunes fired one-shot commitments + stale episodic chatter. Leader-only — followers return a hint to retry at the leader. Returns summarised_groups (how many owner-groups were handed to the summariser, NOT a count of records written) and pruned. Near-duplicate merge verdicts are a separate mechanism, read with dream_recap. Use when the user asks 'consolidate now', 'take a nap', 'forget the stuff that already happened' — or before a memory_recent / commitment_list call when they want fresh state.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {},
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
func newMemorySearchHandler(store *memory.Store, embedder compute.EmbeddingProvider, crossOwner compute.CrossOwnerAuthorizer) compute.BuiltinFunc {
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

		// One audience for both strategies. The substring path used to
		// take none at all, which meant the ownership filter on this
		// tool was really a filter on deployments that had an embedder
		// configured — and the fallback it silently degrades to on an
		// embedding outage returned everyone's records.
		turn, _ := turn.IdentityFrom(ctx)
		audience := compute.ReadAudience(ctx, turn, crossOwner)

		if embedder != nil {
			return runSemanticSearch(ctx, store, embedder, audience, query, tagFilter, limit)
		}
		return compute.RunSubstringSearch(store, audience, query, tagFilter, limit)
	}
}

// runSemanticSearch embeds the query, runs vectorSearch, then
// dereferences the source episodic records for the hits. If
// semantic returns fewer than `limit` hits, augments with
// substring matches — covers pre-embedding records that have
// no vector row yet. Returns fallback-substring on embedder
// failure.
func runSemanticSearch(ctx context.Context, store *memory.Store, embedder compute.EmbeddingProvider, audience memory.Audience, query, tagFilter string, limit int) ([]byte, int, error) {
	vec, err := embedder.EmbedQuery(ctx, query)
	if err != nil {
		payload, _, serr := compute.RunSubstringSearch(store, audience, query, tagFilter, limit)
		return annotateEmbeddingFailure(payload, err), 0, serr
	}
	hits, err := memory.VectorSearch(store, vec, limit*2,
		audience, "", lobslawv1.Retention_RETENTION_UNSPECIFIED)
	if err != nil {
		payload, _, serr := compute.RunSubstringSearch(store, audience, query, tagFilter, limit)
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
			// Re-checked rather than inherited from the vector that
			// pointed here. A legacy or shared vector can carry
			// SourceIds into a private episodic record, and hydration
			// would then hand over the text the vector only indexed.
			if !audience.AllowsEpisodic(&epi) {
				continue
			}
			if tagFilter != "" && !slices.Contains(epi.Tags, tagFilter) {
				continue
			}
			results = append(results, compute.EpisodicToMap(&epi, h.Score()))
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
		more := runSubstringMatches(store, audience, query, tagFilter, limit-len(results), seen)
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
func runSubstringMatches(store *memory.Store, audience memory.Audience, query, tagFilter string, limit int, exclude map[string]bool) []map[string]any {
	tokens := compute.TokeniseQuery(query)
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
		if !audience.AllowsEpisodic(&r) {
			return nil
		}
		if exclude[r.Id] {
			return nil
		}
		if tagFilter != "" && !slices.Contains(r.Tags, tagFilter) {
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
		return compute.TSNano(hits[i].rec.Timestamp) > compute.TSNano(hits[j].rec.Timestamp)
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, compute.EpisodicToMap(h.rec, 0))
	}
	return out
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

// newMemoryWriteHandler returns a compute.BuiltinFunc that writes one
// EpisodicRecord via Raft. The ID is auto-generated (UUID) so the
// model doesn't need to synthesise one. Tags come in as a
// JSON-encoded string array from the LLM's tool-call arguments.

func newMemoryWriteHandler(raft memoryRaftApplier, embedder compute.EmbeddingProvider) compute.BuiltinFunc {
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

		// The model writes here, and what it writes often came from a
		// tool result or a fetched page. Same quarantine rule as
		// ingest: keep the record, keep it out of recall.
		if f, ok := promptguard.Suspicious(event + "\n" + ctxField); ok {
			tags = append(tags, promptguard.Tag(f))
		}

		rec := &lobslawv1.EpisodicRecord{
			Event:      event,
			Context:    ctxField,
			Importance: importance,
			Tags:       tags,
			Retention:  lobslawv1.Retention_RETENTION_LONG_TERM,
		}
		// One door for creating a memory: it stamps ownership from the
		// turn, refuses a record nobody would be able to read, and
		// writes the vector that makes it findable by meaning. This
		// used to assemble and apply the entry here, which is how it
		// came to miss both.
		id, err := memory.Remember(ctx, raft, embedder, 0, rec)
		if err != nil {
			return nil, 1, fmt.Errorf("memory_write: %w", err)
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

// newDreamRecapHandler lists vector records whose SourceIds count
// is > 1 (indicating they are consolidations produced by a dream/REM
// cycle), newest-first. Read-only; safe on followers. Narration
// discipline is enforced prompt-side — the tool returns structured
// JSON; the bot re-renders in voice.
func newDreamRecapHandler(store *memory.Store, authz compute.CrossOwnerAuthorizer) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		// Consolidations summarise the records they were merged from,
		// so a recap of someone else's memories is a summary of their
		// memories. Dream only clusters within an owner, which means
		// each consolidation has exactly one — filtering here is
		// sufficient, and does not need to walk the sources.
		turn, _ := turn.IdentityFrom(ctx)
		audience := compute.ReadAudience(ctx, turn, authz)
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
			if !audience.AllowsVector(&v) {
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
func newMemoryForgetHandler(svc memoryForgetter, crossOwner compute.CrossOwnerAuthorizer) compute.BuiltinFunc {
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
		// Scoped to the caller unless policy has granted them
		// cross-owner reach. Forget is destructive and cascades
		// through consolidations, so an unscoped one lets the model
		// erase another person's memory on request — but an operator
		// tidying up after someone who has left needs exactly that,
		// and refusing it outright is what pushes them to the
		// unauthenticated CLI where nothing records who did it.
		turn, _ := turn.IdentityFrom(ctx)
		req := &lobslawv1.ForgetRequest{
			Query:     query,
			Ids:       ids,
			Tags:      tags,
			Before:    before,
			Requester: compute.ForgetRequester(ctx, turn, crossOwner),
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

// priorEpisodic reads the record being corrected, or nil.
//
// Best-effort by design. A correction whose original has already been
// forgotten is still a correction worth writing, so a miss here
// degrades the metadata rather than failing the call.
func priorEpisodic(store *memory.Store, id string) *lobslawv1.EpisodicRecord {
	if store == nil || id == "" {
		return nil
	}
	raw, err := store.Get(memory.BucketEpisodicRecords, id)
	if err != nil {
		return nil
	}
	var rec lobslawv1.EpisodicRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil
	}
	return &rec
}

// correctedImportance keeps the original's importance unless the
// caller supplies one. A correction is a restatement of the same fact,
// so it is as important as what it replaces.
func correctedImportance(args map[string]string, prior *lobslawv1.EpisodicRecord) int32 {
	if raw, ok := args["importance"]; ok && raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 10 {
			return int32(n)
		}
	}
	if prior != nil && prior.Importance > 0 {
		return prior.Importance
	}
	return 5
}

// correctedTags keeps the original's tags and adds the corrects
// marker, rather than replacing them with it. The old tags are how the
// memory was findable; a correction should not cost that.
func correctedTags(prior *lobslawv1.EpisodicRecord, oldID string) []string {
	out := []string{"corrects:" + oldID}
	if prior == nil {
		return out
	}
	for _, t := range prior.Tags {
		if strings.HasPrefix(t, "corrects:") {
			continue // not a chain of every ancestor
		}
		out = append(out, t)
	}
	return out
}

// correctedRetention keeps the original's retention tier. Forcing
// long-term promoted a short-term memory the operator had scoped
// deliberately.
func correctedRetention(prior *lobslawv1.EpisodicRecord) lobslawv1.Retention {
	if prior != nil && prior.Retention != lobslawv1.Retention_RETENTION_UNSPECIFIED {
		return prior.Retention
	}
	return lobslawv1.Retention_RETENTION_LONG_TERM
}

// newMemoryCorrectHandler writes a new memory with superseded
// metadata, then forgets the original by id. Two-step operation
// but single transactional intent: audit log retains both the new
// write and the forget, preserving the correction trail.
func newMemoryCorrectHandler(store *memory.Store, raft memoryRaftApplier, forgetter memoryForgetter, crossOwner compute.CrossOwnerAuthorizer, embedder compute.EmbeddingProvider) compute.BuiltinFunc {
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

		// The record being corrected, read for its metadata. Absent or
		// unreadable is not fatal: the correction still stands, it
		// just carries defaults rather than the original's importance
		// and tags.
		prior := priorEpisodic(store, oldID)

		// Step 1: write the correction as a new memory with a
		// "corrects:<old_id>" tag so the audit trail is queryable.
		newRec := &lobslawv1.EpisodicRecord{
			Event:   newEvent,
			Context: newContext,
			// Carried from the original where the caller did not say
			// otherwise. Hardcoding importance 5 silently demoted a
			// correction to an importance-9 memory, and replacing the
			// tag list dropped every topic tag the old record was
			// findable by — a correction that makes a memory harder to
			// find has undone more than it fixed.
			Importance: correctedImportance(args, prior),
			Tags:       correctedTags(prior, oldID),
			Retention:  correctedRetention(prior),
		}
		// Same door as memory_write. An unowned correction would be
		// unreadable, and the forget below is scoped to the caller —
		// so it would replace a readable memory with an invisible one
		// and report success.
		newID, err := memory.Remember(ctx, raft, embedder, 0, newRec)
		if err != nil {
			return nil, 1, fmt.Errorf("memory_correct: %w", err)
		}

		// Step 2: forget the original. Any consolidations containing
		// the old id are also swept (privacy-safe).
		turn, _ := turn.IdentityFrom(ctx)
		forgetReq := &lobslawv1.ForgetRequest{
			Ids:       []string{oldID},
			Requester: compute.ForgetRequester(ctx, turn, crossOwner),
		}
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

// newDreamNapHandler runs one Dream/REM pass on demand. Wraps
// memory.Service.Dream so the agent can consolidate + prune without
// waiting for the next scheduled cycle. Leader-routed by the service;
// followers surface FailedPrecondition with the leader address.
func newDreamNapHandler(svc memoryDreamer) compute.BuiltinFunc {
	return func(ctx context.Context, _ map[string]string) ([]byte, int, error) {
		resp, err := svc.Dream(ctx, &lobslawv1.DreamRequest{})
		if err != nil {
			return nil, 1, fmt.Errorf("dream_nap: %w", err)
		}
		// "summarised_groups", not "consolidated".
		//
		// Two different things in this system were called consolidation
		// and the collision was doing real damage: this count is
		// owner-groups handed to the summariser, while dream_recap and
		// `lobslaw memory consolidations` read the near-duplicate
		// ADJUDICATION log, which a summarisation pass does not write
		// to. A nap could truthfully report "consolidated 5" and the
		// recap truthfully report nothing, and the pair is unreadable —
		// an agent asked to explain it said so, and could only report
		// that it could not reconcile the two.
		out, err := json.Marshal(map[string]any{
			"summarised_groups": resp.Consolidated,
			"pruned":            resp.Pruned,
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
func newMemoryRecentHandler(store *memory.Store, authz compute.CrossOwnerAuthorizer) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		// memory_recent walks the episodic bucket directly rather than
		// going through vector search, so it needs the audience applied
		// here. This is the same leak the substring path had: scoping
		// the vector index does not scope a reader that never touches
		// it.
		turn, _ := turn.IdentityFrom(ctx)
		audience := compute.ReadAudience(ctx, turn, authz)
		limit := 20
		if raw, ok := args["limit"]; ok && raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 50 {
				limit = n
			}
		}
		retentionFilterEnum, retErr := types.ParseRetention(args["retention"])
		if retErr != nil {
			return nil, 2, retErr
		}
		retentionFilter := types.RetentionString(retentionFilterEnum)

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
			if !audience.AllowsEpisodic(&rec) {
				return nil
			}
			if retentionFilterEnum != lobslawv1.Retention_RETENTION_UNSPECIFIED && rec.Retention != retentionFilterEnum {
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
				Retention:  types.RetentionString(rec.Retention),
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
