package compute

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/jmylchreest/lobslaw/pkg/textutil"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"

	"github.com/jmylchreest/lobslaw/pkg/promptgen"

	"github.com/jmylchreest/lobslaw/internal/promptguard"
)

// ContextEngine assembles per-turn contextual additions to the
// system prompt: semantic memory recall, heuristic tool filtering,
// and eventually a preflight classifier that routes to a cheap
// model before the main turn.
//
// Openclaw's ContextEngine is the design reference; lobslaw's
// version is narrower because we don't have a session-DAG yet.
// What we do have: vector-backed episodic memory, a tool
// registry, and a multi-provider RoleMap — enough to compute
// "relevant memory + likely-useful tools" per turn.
type ContextEngine struct {
	store      *memory.Store
	embedder   EmbeddingProvider
	crossOwner CrossOwnerAuthorizer
	log        *slog.Logger

	maxRecall int
}

// ContextEngineConfig wires the engine. A nil store or embedder
// disables the corresponding feature silently — partially
// configured deployments still benefit from whichever primitive
// they have.
type ContextEngineConfig struct {
	Store    *memory.Store
	Embedder EmbeddingProvider
	Logger   *slog.Logger

	// CrossOwner decides whether this turn's caller may recall across
	// owners. Nil never widens — see readAudience. Passive recall is
	// the most dangerous place to get this wrong, because it runs with
	// no tool call in front of it and its output lands in the system
	// prompt rather than anywhere the user can see it.
	CrossOwner CrossOwnerAuthorizer

	// MaxRecall caps the number of memory records injected into
	// the prompt per turn. 3 is the sweet spot — enough for
	// continuity without drowning the turn in stale context.
	MaxRecall int
}

// NewContextEngine is safe to call with an empty config; the
// engine will no-op where primitives are missing.
func NewContextEngine(cfg ContextEngineConfig) *ContextEngine {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxRecall := cfg.MaxRecall
	if maxRecall <= 0 {
		maxRecall = 3
	}
	return &ContextEngine{
		store:      cfg.Store,
		embedder:   cfg.Embedder,
		crossOwner: cfg.CrossOwner,
		log:        logger,
		maxRecall:  maxRecall,
	}
}

// ContextAssembly is the output of the engine's per-turn run.
//
// Blocks rather than a rendered string, so recall goes through
// promptgen.WrapContext like every other untrusted input. Rendering
// here meant a second implementation of the delimiter contract — a
// bespoke <relevant_context> tag that BuildSafety never trained the
// model on — free to drift from the one the safety block teaches.
//
// RecallIDs carries the episodic record ids so callers can cite or
// track retrieval rate without re-scanning.
type ContextAssembly struct {
	Blocks    []promptgen.ContextBlock
	RecallIDs []string
}

// Rendered returns the recall blocks as one wrapped string, or empty
// when nothing was recalled.
func (a ContextAssembly) Rendered() string {
	if len(a.Blocks) == 0 {
		return ""
	}
	return promptgen.WrapContext(a.Blocks)
}

// Assemble runs per-turn recall against the user message and
// returns the additions to fold into the system prompt. Failures
// degrade silently — a turn with no recall is still useful; a
// turn that crashes on recall is useless.
func (e *ContextEngine) Assemble(ctx context.Context, userMessage string) ContextAssembly {
	userMessage = strings.TrimSpace(userMessage)
	if userMessage == "" || e.store == nil {
		return ContextAssembly{}
	}

	// Scoped to the turn's caller. This path is the reason Audience is a
	// required argument: passive recall runs on every turn with no tool
	// call in front of it, so an unscoped search here puts one user's
	// memories into another user's system prompt before they have said
	// anything. An unidentified turn yields the zero Principal, which
	// still sees shared and legacy records but nothing owned.
	//
	// A caller policy has granted cross-owner read widens to
	// Everyone(); nothing else does, including a caller who merely
	// holds role:operator.
	turn, _ := TurnIdentityFrom(ctx)
	audience := readAudience(ctx, turn, e.crossOwner)

	entries, strategy := e.recall(ctx, audience, userMessage)
	if len(entries) == 0 {
		return ContextAssembly{}
	}
	e.log.Debug("context-engine: recall", "strategy", strategy, "hits", len(entries))

	// Deterministic render order: higher score first, then
	// higher importance, then newer.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].score != entries[j].score {
			return entries[i].score > entries[j].score
		}
		if entries[i].rec.Importance != entries[j].rec.Importance {
			return entries[i].rec.Importance > entries[j].rec.Importance
		}
		return tsNano(entries[i].rec.Timestamp) > tsNano(entries[j].rec.Timestamp)
	})

	ids := make([]string, 0, len(entries))
	blocks := make([]promptgen.ContextBlock, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.rec.Id)
		// score and when ride on Source, which WrapContext already
		// renders as an attribute — so the metadata survives without a
		// second tag vocabulary to carry it.
		source := fmt.Sprintf("memory:recall score=%.3f", e.score)
		if e.rec.Timestamp != nil {
			source += fmt.Sprintf(" when=%s", e.rec.Timestamp.AsTime().Format("2006-01-02 15:04"))
		}
		blocks = append(blocks, promptgen.ContextBlock{
			Source:   source,
			Category: promptgen.CategoryLongTerm,
			Trust:    promptgen.TrustUntrusted,
			Content:  truncateContext(e.rec.Context, 800),
		})
	}

	return ContextAssembly{Blocks: blocks, RecallIDs: ids}
}

// recallEntry is one memory chosen for the prompt, with the score
// that chose it. The score's SCALE depends on the strategy — cosine
// for semantic, matched-token fraction for lexical — which is
// harmless because a turn uses exactly one strategy, and both are
// 0..1 so the rendered attribute stays readable either way.
type recallEntry struct {
	rec   *lobslawv1.EpisodicRecord
	score float32
}

// recall picks a strategy and returns what it found, plus the name of
// the strategy for the log.
//
// EVERY embedding-shaped failure lands on lexical — embedder absent,
// embed failed, or vector search errored. Returning an empty assembly
// instead fails silently on every turn: a node with no [embeddings]
// block would run permanently with no passive recall and say nothing
// about it, and an embedding outage would do the same temporarily.
// Lexical recall is worse than
// semantic — it cannot match a paraphrase — but it is a great deal
// better than nothing, and memory_search has had exactly this fallback
// all along.
func (e *ContextEngine) recall(ctx context.Context, audience memory.Audience, userMessage string) ([]recallEntry, string) {
	if e.embedder == nil {
		return e.lexicalRecall(audience, userMessage), "lexical (no embedder configured)"
	}
	// EmbedQuery, not Embed: this is the user's question, and an
	// asymmetric model wants the query prefix on it rather than the
	// passage one.
	vec, err := e.embedder.EmbedQuery(ctx, userMessage)
	if err != nil {
		// WARN rather than Debug: an embedding outage silently
		// downgrades every turn's recall, and the operator needs to
		// see it to diagnose (wrong API key, provider blocklist, dim
		// mismatch, etc.).
		e.log.Warn("context-engine: embed failed; falling back to lexical recall", "err", err)
		return e.lexicalRecall(audience, userMessage), "lexical (embed failed)"
	}
	entries, err := e.vectorRecall(audience, vec)
	if err != nil {
		e.log.Warn("context-engine: vector search failed; falling back to lexical recall", "err", err)
		return e.lexicalRecall(audience, userMessage), "lexical (vector search failed)"
	}
	// ZERO HITS IS A SUCCESS, and that is the trap.
	//
	// vectorSearch skips records whose embedding width differs from the
	// query's — deliberately, with a warning — and returns no error. So
	// the two moments a corpus is most likely to be unsearchable both
	// arrive here as a clean empty result: the day embeddings are first
	// enabled and every existing record has no vector at all, and the
	// day the model changes width and every old vector is skipped.
	//
	// Falling back costs one bucket scan on a genuine miss. Not falling
	// back costs every memory the node holds, silently, until somebody
	// notices recall has stopped working.
	if len(entries) == 0 {
		if lex := e.lexicalRecall(audience, userMessage); len(lex) > 0 {
			return lex, "lexical (no vector hits)"
		}
		return nil, "semantic (no hits)"
	}
	return entries, "semantic"
}

// vectorRecall is the semantic path: search the vector store, then
// dereference each hit to the episodic records it summarises.
func (e *ContextEngine) vectorRecall(audience memory.Audience, vec []float32) ([]recallEntry, error) {
	hits, err := memory.VectorSearch(e.store, vec, e.maxRecall*2,
		audience, "", lobslawv1.Retention_RETENTION_UNSPECIFIED)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	entries := make([]recallEntry, 0, e.maxRecall)
	for _, h := range hits {
		for _, sid := range h.Record().SourceIds {
			if seen[sid] {
				continue
			}
			seen[sid] = true
			raw, err := e.store.Get(memory.BucketEpisodicRecords, sid)
			if err != nil {
				continue
			}
			var epi lobslawv1.EpisodicRecord
			if err := proto.Unmarshal(raw, &epi); err != nil {
				continue
			}
			// Re-checked rather than inherited from the vector that
			// pointed here: a legacy or shared vector can carry
			// SourceIds into a private episodic record, and this is
			// the path that puts recalled text into the system prompt.
			if !audience.AllowsEpisodic(&epi) {
				continue
			}
			if e.quarantined(&epi) {
				continue
			}
			entries = append(entries, recallEntry{rec: &epi, score: h.Score()})
			if len(entries) >= e.maxRecall {
				break
			}
		}
		if len(entries) >= e.maxRecall {
			break
		}
	}
	return entries, nil
}

// lexicalRecall is the fallback path: token overlap against the
// episodic records directly, with no vector store in the way.
//
// It reaches episodic records WITHOUT the SourceIds dereference the
// vector path needs, because it matches their text in the first
// place. That also means it can find records written before any
// embedder existed, which the semantic path cannot.
func (e *ContextEngine) lexicalRecall(audience memory.Audience, userMessage string) []recallEntry {
	// Over-fetch for the same reason the vector path does: the
	// quarantine filter below can empty out an otherwise full page.
	hits, err := lexicalEpisodicSearch(e.store, audience, userMessage, "", e.maxRecall*2)
	if err != nil {
		e.log.Warn("context-engine: lexical recall failed", "err", err)
		return nil
	}
	tokens := len(tokeniseQuery(userMessage))
	if tokens == 0 {
		return nil
	}
	entries := make([]recallEntry, 0, e.maxRecall)
	for _, h := range hits {
		if e.quarantined(h.rec) {
			continue
		}
		// Normalised to 0..1 so the rendered score= attribute means
		// the same kind of thing it does on the semantic path.
		entries = append(entries, recallEntry{
			rec:   h.rec,
			score: float32(h.score) / float32(tokens),
		})
		if len(entries) >= e.maxRecall {
			break
		}
	}
	return entries
}

// quarantined reports whether a record was flagged at ingest.
//
// The record is KEPT — it is usually the evidence — but recall is the
// path that replays it into a prompt on every later turn, which is the
// whole reason it was flagged. Both recall strategies check it; the
// memory_search tool deliberately does not, because there the model
// asked for it by name.
func (e *ContextEngine) quarantined(rec *lobslawv1.EpisodicRecord) bool {
	if !promptguard.IsQuarantined(rec.Tags) {
		return false
	}
	e.log.Warn("context engine: skipping quarantined record in recall",
		"id", rec.Id, "tags", rec.Tags)
	return true
}

func truncateContext(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return textutil.Truncate(s, "…", max)
}
