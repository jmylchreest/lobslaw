# Memory

How `internal/memory` provides persistent, semantically-searchable, self-rationalising memory for the agent.

## TL;DR

Buckets persisted through Raft:

- **VectorRecords** — dense embeddings + text. Semantic recall.
- **EpisodicRecords** — structured events with tags, importance, timestamps. Dream/REM consolidation source.
- **Sessions / SessionMessages** — durable conversation transcripts. See [Sessions](#sessions) below.

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

## Flow diagrams

Full sequence diagrams for the Dream cycle (with Phase 2 merge) and Forget cascade live in [ARCHITECTURE.md](ARCHITECTURE.md) since they span component boundaries. The composition workflows below describe the same flows in caller-side code.

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

## Sessions

A **session** is the durable transcript of one conversation on one channel. It is deliberately *not* memory:

| | Episodic memory | Session transcript |
|---|---|---|
| Holds | what the agent chose to remember | every message, verbatim |
| Written by | the agent, via `memory_write` / turn ingest | the channel, every turn, unconditionally |
| Scored / consolidated | yes (Dream) | never |
| Read for | semantic recall across time | replaying *this* conversation |
| Forgettable | yes, by content match | only by dropping the whole thread |

Both exist because they answer different questions. "What do I know about James's deploy preferences?" is memory. "What did we just say to each other?" is a session. Before sessions, the second question was answered by a per-process in-memory map that died on restart.

### Storage layout

```
BucketSessions          key: "<channel>:<channel_id>"                 → SessionRecord
BucketSessionMessages   key: "<channel>:<channel_id>:<seq, %020d>"    → SessionMessage
```

The index record carries the retained range (`first_seq`, `next_seq`); message bodies live in their own bucket. Two consequences worth knowing:

- **Zero-padding is load-bearing.** bbolt orders keys bytewise, so `%020d` makes lexical order identical to sequence order. Without it, message 10 sorts before message 9 and a transcript silently scrambles on the tenth message.
- **The `:` separator is load-bearing too.** The message prefix is `"<session_id>:"` *including* the trailing colon — otherwise session `rest:1` would prefix-match `rest:10`'s transcript. Channel and channel-id are both rejected if they contain `:`, same rule as channel state.

Reads use `Store.ForEachPrefix`, which cursor-seeks straight to the range rather than decrypting every message in the cluster to find one conversation.

**Who a channel-id belongs to is the channel's business.** The store enforces the key shape and nothing about ownership, because ownership isn't uniform: a Telegram channel-id is a *chat*, deliberately shared by every member of a group, while a REST channel-id is `"<escaped-user>/<client session_id>"` so two callers picking the same id get two conversations. A `rec.UserId` check in the store would break the first case to fix the second. Channels that mint ids from client input scope them; channels whose ids are already an address don't need to. What the *agent* may read across those conversations is a separate question, answered in [Transcript visibility](#transcript-visibility).

### Append is one raft entry per turn

`SessionAppendRecord` bundles the updated index record, the turn's messages, and the keys the trim evicted. The FSM applies evictions, then bodies, then the index record last — so a crash mid-apply leaves unreferenced messages (harmless; the next trim reclaims them) rather than an index promising messages that aren't there.

**Trimming is computed on the leader and shipped inside the entry.** The FSM has to be deterministic across live apply and log replay; recomputing "which messages are too old" at apply time would make replay depend on state the original apply didn't see. Same class of bug as the claim-expiry one documented in `fsm.applyClaim`.

System messages are dropped on write: promptgen rebuilds the system prompt every turn from live SOUL state, the tool list, and the current time, so a persisted copy would replay a stale identity.

### Leader-only, with a degraded mode

Session writes go through raft and are therefore leader-only, like every other write in this package — there's no forwarding layer. A turn handled on a follower gets `memory.ErrNotLeader`.

The gateway does not treat that as fatal. `conversationLog` keeps an in-memory cache in front of the durable store; a failed durable write leaves the conversation coherent for the life of that process, and the durable copy resumes once leadership settles. A durable read that returns *nothing* also falls through to the cache, which is what stops a leadership change mid-conversation from looking like amnesia.

The honest limitation: on a follower, history still doesn't survive a restart. Leader forwarding is the fix and isn't built.

### Compaction

`SessionRecord` carries a running `summary` plus `summary_through_seq`. The
memory layer only stores it — the summariser is an LLM call and lives in
`internal/compute`, per the deterministic/interpretive split at the top of this
document. `compute.Compactor` is the caller that composes the two.

`LoadTranscript` returns the summary and only the messages after it, so the
compacted head is represented once rather than twice.

### Search and titles

`SearchTranscripts` is a **substring** scan, not semantic. Episodic memory
already embeds every turn and answers "what do I know about X" through
`memory_search`; what it cannot do is find the exact words in the exact thread —
a command that was run, an error string, a name. Building a second embedding
pipeline over the same content would duplicate the cost for a worse version of a
capability we already have.

Results are ranked most-recently-active first: when several threads mention the
same thing, the live one is nearly always the one meant. Snippets are windowed
around the match so a 10 MB tool result doesn't arrive in the agent's context.

`Title` is generated once, on a conversation's first compaction — the summary is
exactly the input a title needs, and a conversation long enough to compact is
long enough to be worth finding again.

### Transcript visibility

The store holds every conversation the node has ever had, from every user of
every channel. The agent's `session_search` / `session_list` / `session_read`
tools read across all of it, so they are scoped per turn. The rule, implemented
by `compute.TurnIdentity.Visible`:

1. **The conversation the turn is in is always readable** — same `channel` *and*
   `channel_id` as the current turn, whoever the record says opened it.
2. **Any other conversation is readable only when its `user_id` matches the
   turn's caller.**

Clause 1 is not a loophole, it's the group-chat case: Telegram shares one
session across every member, and `rec.UserId` is whoever spoke first. Ownership
alone would refuse the second member the conversation they are visibly having.
Clause 2 is what stops a shared bot handing user B snippets of user A's threads
— `UserIDScopes` in the Telegram config exists precisely because a node often
has more than one user.

#### Where identity comes from

`TurnIdentity` travels on the request context, attached once per turn in
`Agent.runLoop`, and is the only source of caller identity for any builtin —
not just the session tools. It carries the user, their permission scope, the
conversation address, and the timezone.

It is deliberately *not* in the tool-argument map. That map is built from the
model's own JSON, so a value read out of it is a value the model can choose.
This was not hypothetical: identity used to be injected there as synthetic
`__user_id` / `__chat_id` keys, and the injection was conditional on the request
carrying each field — so on a turn with no channel origin (a scheduled task, a
webhook, a research worker) the model's own value survived. `notify` chose whose
devices to ring from it, `commitment_create` chose whose chat a reminder fired
into, and `oauth_start` stamped who initiated a credential flow into the audit
log. `__scope` was never injected by anything, so the scope prefix on that audit
field could only ever have come from the model.

Scrubbing the map before injecting would have closed those instances without
closing the class: trusted and untrusted values would still share one namespace,
separated by a naming convention a new contributor has no way to discover. A
context value cannot be reached from inside the model's output at all.

`TestBuiltinsDoNotReadIdentityFromArgs` parses this package and fails on any
handler that reads a retired identity key out of a map, so the invariant is
enforced rather than remembered.

Three properties worth keeping:

- **Identity travels on the context** (`compute.WithSessionScope`, attached once
  per turn in `Agent.runLoop`), never in the tool arguments. Tool args come from
  the model, and the synthetic `__user_id` / `__chat_id` args are only
  overwritten when the request carries those fields — good enough for
  attribution, not for an authorisation decision. Same reasoning as `clampArg`:
  the model does not widen its own reach by asking.
- **Filtering happens before the result limit**, which is why
  `SessionBrowseQuery.Visible` and `SessionBrowser.Recent` take a predicate
  rather than being filtered by their caller. Filter afterwards and a busy
  shared node fills the window with threads the caller can't see and then
  discards them, so their own results vanish.
- **A refused `session_read` and a non-existent conversation give the same
  answer.** Distinguishing them turns the tool into an oracle for "does user X
  have a thread with chat id Y", and chat ids are guessable.

A context with *no* scope is unscoped and sees everything. That is deliberate,
and it is only safe because of who can reach it: the agent loop attaches a scope
to every turn, including anonymous ones, so nothing the model drives arrives
without one. Bare contexts come from operator tooling (`cmd/inspect` reads the
store directly) and the compactor — callers that already hold the whole
database. Any new driver of these builtins must attach a scope.

Known consequence: scope identity is `Claims.UserID`, which is per-channel
(`tg-@alice`, a REST subject), not a cluster-wide person. A solo operator using
both Telegram and the REST API has two identities and will not find one
channel's threads from the other. Cross-identity aliasing is a mapping we don't
have; a false negative costs recall, a false positive is the leak.

### Retention

`gateway.session_max_messages` (default 200) caps messages per session; the oldest are evicted first. This is a **storage** bound — raising it costs disk and raft-snapshot size, not tokens per turn. What reaches the model is `defaultSessionTail` (100) messages, and eventually whatever conversation compaction leaves behind.

Storage and context are deliberately decoupled: the store keeps the full
retained transcript (for search and export) while only the summary plus a short
verbatim tail reaches the model. Idle sessions are still never expired — the
message cap is the only storage bound today.

---

## Upstream tracking

No active Go proposals that would simplify this architecture today. HNSW-backed vector search (post-MVP upgrade path for FindClusters + Search over larger stores) is tracked in DEFERRED.md.

Phase 5 (Agent Core) is the next phase that materially changes memory use — it lands the first real Adjudicator (LLM-backed) and the Reranker interface for hot-path recall. Memory-service primitives defined here should not change shape.
