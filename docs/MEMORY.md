# Memory

How `internal/memory` provides persistent, semantically-searchable, self-rationalising memory for the agent.

## TL;DR

Two buckets persisted through Raft:

- **VectorRecords** — dense embeddings + text. Semantic recall.
- **EpisodicRecords** — structured events with tags, importance, timestamps. Dream/REM consolidation source.

Every operation distinguishes **deterministic primitives** (cheap math — Search, FindClusters, Forget) from **LLM interpretation** (Summarizer, Adjudicator, Reranker). Callers compose the two layers into workflows. The memory service never calls an LLM directly; the LLM layer never writes to the store directly. Hard boundary.

## Architectural split

```
┌─ Memory service (deterministic) ─┐      ┌─ LLM layer (interpretive) ─┐
│  Store    Recall                  │      │  Summarizer                 │
│  Search   FindClusters            │      │  Adjudicator                │
│  Forget   EpisodicAdd   Dream     │      │  Reranker (Phase 5)         │
└───────────┬──────────────────────┘      └────────────┬────────────────┘
            │                                          │
            └──────────── Caller orchestration ────────┘
              Agent loop / DreamRunner / Channel handlers
```

**Why the split**: cost opacity, testability, injectability. A caller that only needs cheap candidate retrieval shouldn't pay for an LLM call. Tests exercising merge-flow plumbing shouldn't need an LLM mock. Different callers may inject different LLM strategies (cheap-fast for hot-path rerank; smart-expensive for merge adjudication).

See aide decision `lobslaw-memory-merge-architecture` for the full rationale.

---

## Deterministic primitives

### Store / Recall / EpisodicAdd

Persist VectorRecords and EpisodicRecords via Raft (`raft.Apply`). Recall reads by ID from the local store directly — no Raft round-trip for reads. See `internal/memory/service.go`.

### Search — vector cosine similarity

Takes a query embedding, scans the vector bucket, returns top-K by cosine similarity. Scope/retention filters apply during the scan (records failing a filter are never scored). O(N × D) where D is embedding dimension. Personal-scale-acceptable; HNSW upgrade tracked in DEFERRED.md.

**No text-based search.** `SearchRequest.Text` returns `Unimplemented` — the caller computes the embedding via Phase 5's Provider Resolver, then calls Search with that embedding.

### FindClusters — pairwise cosine + union-find

New in Phase 3.4. Discovers groups of near-duplicate records without an input query.

```go
clusters, _ := mem.FindClusters(ctx, &FindClustersRequest{
    Threshold:       0.88,          // cosine floor
    MinClusterSize:  2,             // skip singletons
    MaxClusterSize:  10,            // split hairballs
    RetentionFilter: "long-term",   // session records never merge
    ScopeFilter:     "alice",
})
```

- Pure math; no LLM dependency.
- Filter applied during the scan — mixed-retention records never cross-cluster.
- Consolidated records (`SourceIds` non-empty) are skipped — we cluster sources, not summaries.
- Dense components exceeding `MaxClusterSize` are split by nearest-first greedy chunking, preserving the tightest neighbour pairs.
- `Cluster.Id` is a stable SHA-256 of sorted member IDs — lets audit logs correlate re-observations across Dream runs.
- `MinSimilarity` / `AvgSimilarity` populated from intra-cluster edges only.
- Output sorted by descending average similarity.

### Forget — full-text, tag, timestamp, or explicit IDs

```go
// Topic-based forget (Phase 3.2):
mem.Forget(ctx, &ForgetRequest{Query: "medical", Tags: []string{"health"}})

// Explicit IDs (Phase 3.4a) — how Search → preview → Forget composes:
hits := mem.Search(ctx, &SearchRequest{Embedding: q, Limit: 50})
// ... client-side preview / confirmation ...
mem.Forget(ctx, &ForgetRequest{Ids: idsOf(hits.Hits)})
```

Deletion cascades via `SourceIds` — any consolidated record whose sources intersect the matched set is also deleted. See aide decision `lobslaw-forget-cascade` for the "aggressive sweep" rationale (forgetting a source while keeping the summary would leak the forgotten content via the summary's embedding).

At least one of `query`/`before`/`tags`/`ids` must be set — the handler refuses "forget everything".

---

## LLM interpretation interfaces

All defined in `internal/memory/` but have no memory-service dependencies. Each takes context + data, returns a decision. Phase 5 ships the first real implementations.

### Summarizer

```go
type Summarizer interface {
    Summarize(ctx, events []string) (summary string, embedding []float32, err error)
}
```

Consolidates a batch of episodic records into a narrative. Called during Dream's consolidation phase. `nil` makes Dream skip summarisation.

### Adjudicator

```go
type Adjudicator interface {
    AdjudicateMerge(ctx, cluster *Cluster) (MergeDecision, error)
}
```

Decides what to do with a near-duplicate cluster. Four verdicts:

| Verdict | Action | Destructive? |
|---|---|---|
| `KeepDistinct` | Do nothing | No |
| `Merge` | Store consolidated, delete originals | **Yes** |
| `Conflict` | Tag `metadata[conflict-cluster] = <id>`, preserve all | No |
| `Supersedes` | Tag `metadata[supersedes-chain] = <id>`, preserve all | No |

**Critical invariant: on error, callers treat the cluster as `KeepDistinct`**. False-merge is irreversible; false-no-merge is just bloat. The `AlwaysKeepDistinctAdjudicator` stub is the boot-default — nothing merges at runtime until Phase 5 plugs in a real Adjudicator via `DreamRunner.SetAdjudicator`.

### Reranker (Phase 5)

```go
// Shape proposed; not yet implemented.
type Reranker interface {
    Rerank(ctx, query string, candidates []*VectorRecord, topN int) ([]RerankResult, error)
}
```

The second stage of two-stage RAG. Vector `Search` is high-recall/cheap (cosine can't reason about intent, negation, temporal qualifiers); LLM rerank over top-K candidates is high-precision. Lands with Phase 5's Agent Core when the agent loop needs to select memory for system-prompt injection.

---

## Composition workflows

### Hot-path recall (Phase 5)

```go
cands, _ := mem.Search(ctx, &SearchRequest{Embedding: qEmb, Limit: 50})
top, _ := reranker.Rerank(ctx, userQuery, cands.Hits, 10)
systemPrompt := promptgen.BuildContext(top)
```

Agent composes cheap retrieval + expensive semantic filtering. Memory service sees only the Search call.

### Dream-time merge (Phase 3.4, landed)

```go
// DreamRunner.Run → after summarise → after prune:
clusters := mem.FindClusters(ctx, retention="long-term")
for each cluster {
    decision := adjudicator.AdjudicateMerge(cluster)    // LLM or stub
    switch decision.Verdict {
    case Merge:        mem.Store(consolidated); mem.Forget(ids=originals)
    case Conflict:     tag each member with conflict-cluster:<id>
    case Supersedes:   tag each member with supersedes-chain:<id>
    case KeepDistinct: // no-op (safe default)
    }
}
```

Error paths at every step are conservative. `findClusters` error → phase logs + skips, Dream continues. `AdjudicateMerge` error on one cluster → skip that cluster, continue. `applyMerge` or `tagCluster` error → log, next run retries.

### User-initiated topic forget (Phase 6)

```go
// Channel handler (REST / Telegram / CLI):
emb := provider.Embed(ctx, userQuery)
hits := mem.Search(ctx, &SearchRequest{Embedding: emb, Limit: 50})
// Render preview: "I'd delete these 27 records. Confirm?"
if user.confirmed() {
    mem.Forget(ctx, &ForgetRequest{Ids: idsOf(hits)})
}
```

No dedicated `ForgetSemantic` RPC — the composition of `Search → Forget(ids)` covers it. The preview UI is medium-specific (Telegram buttons, REST JSON, CLI prompt) so it belongs at the channel layer, not the server.

---

## Retention tiers

| Tier | Source | Dream treatment |
|---|---|---|
| `session` | Tool outputs, transient context | Pruned aggressively; NOT merge-eligible |
| `episodic` | User turns on channels | Scored + consolidated; NOT merge-eligible today |
| `long-term` | Explicit "remember this" or consolidation output | Never auto-pruned; **only tier that participates in merge** |

`mergePhase` filters to `long-term` only. Session chatter can never accidentally be consolidated into persistent memory.

---

## Forget semantics

Aggressive by design:

1. Match sources by query / tags / before / explicit IDs.
2. Find all consolidated records whose `SourceIds` intersect the matched set.
3. Delete matched sources AND all intersecting consolidations (not just the overlapping SourceIDs — the whole consolidation).

A consolidation retains its source's content in both text and embedding space, so "forgetting one source and re-consolidating" would leak the forgotten input via the surviving summary. Deleting the whole consolidation is the safe choice. The next Dream run rebuilds consolidations from surviving sources, so there's no permanent information loss — only rebuild cost paid lazily.

See aide decision `lobslaw-forget-cascade`.

---

## Upstream tracking

No active Go proposals that would simplify this architecture today. HNSW-backed vector search (post-MVP upgrade path for FindClusters + Search over larger stores) is tracked in DEFERRED.md.

Phase 5 (Agent Core) is the next phase that materially changes memory use — it lands the first real Adjudicator (LLM-backed) and the Reranker interface for hot-path recall. Memory-service primitives defined here should not change shape.
