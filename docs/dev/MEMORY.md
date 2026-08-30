# Memory

How `internal/memory` provides persistent, semantically-searchable, self-rationalising memory for the agent.

## TL;DR

Buckets persisted through Raft:

- **VectorRecords** — dense embeddings + text. Semantic recall.
- **EpisodicRecords** — structured events with tags, importance, timestamps. Dream/REM consolidation source.
- **Sessions / SessionMessages** — durable conversation transcripts. See [Sessions](#sessions) below.

Every operation distinguishes **deterministic primitives** (cheap math — Search, FindClusters, Forget) from **LLM interpretation**. Summarizer is the only one of those built. Adjudicator and Reranker were designed and their scaffolding removed in 2026-08 — see the note under each — because scaffolding that never runs reads as a feature to anyone grepping for it. Callers compose the two layers into workflows. The memory service never calls an LLM directly; the LLM layer never writes to the store directly. Hard boundary.

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

### Adjudicator (design; scaffolding removed 2026-08)

The interface, the `AlwaysKeepDistinctAdjudicator` stub, `mergePhase` and
the cluster tagging were removed. Nothing installed a real Adjudicator,
so every Dream run clustered the corpus and then discarded each verdict
as `KeepDistinct` — a similarity pass over long-term memory, nightly,
for no effect. The `conflict-cluster` and `supersedes-chain` tags it
wrote were read by nothing.

`BucketConsolidations`, its FSM case and `lobslaw memory consolidations`
were KEPT: they read records already written, and dropping a replicated
bucket is a migration rather than a cleanup.

The design below is retained as the intended shape, not a description
of code that exists.

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

**Critical invariant, when this is built: on error, callers treat the cluster as `KeepDistinct`**. False-merge is irreversible; false-no-merge is just bloat.

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

### Consolidation log

Dream merges, supersedes and prunes memories on its own schedule. The
verdict and the reason were computed on every run and then written to a
log line and forgotten, so "why did it merge those two notes" had no
answer and "what has it been doing to my memory" had none either.

Memory that silently rewrites itself and cannot be inspected is a trust
problem for a privacy-first product, so every adjudication is now a
durable record (`BucketConsolidations`):

```
lobslaw memory consolidations
lobslaw memory consolidations --verdict merge --since 168h
lobslaw memory consolidations --owner user:alice --full
```

Offline, like the rest of `lobslaw memory`.

**Every verdict is recorded, including `keep_distinct`.** "Why did it
*not* merge these" gets asked as often as the opposite, and a log of
changes alone cannot answer it.

**A decision that failed to apply is recorded as attempted**, with the
error. That is precisely when a user notices something is wrong, and a
log that showed nothing would hide the case it exists for.

**Source ids outlive the originals.** After a merge the log is the only
remaining record of what went into the consolidation.

**Owner-scoped**, like the records themselves — a consolidation log
that leaked across owners would describe one person's memories to
another.

Bounded by 90 days or 5000 entries, whichever bites first, pruned by
Dream itself. A second scheduled task to tidy after the first is one
more thing to misconfigure.

## Dream-time merge (Phase 3.4, landed)

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

### Why both, rather than one pointing at the other

Every turn's text is therefore written twice: an `EpisodicRecord` (lossy, scored,
consolidated by Dream, pruned at 24h under `RETENTION_SESSION`) and a
`SessionMessage` per message (verbatim, ordered, capped). The obvious
simplification is to collapse that — episodic keeps the embedding plus a
`session_id:seq` pointer, and the text lives once.

It was considered and rejected, because it inverts the dependency. Memory is
meant to outlive the conversation — that is the whole point of the split — and
under a pointer a durable consolidated memory would depend on a transcript that
is capped, evicted oldest-first, and hard-deleted by `Forget`. "Forget this
thread" would then blow holes in memories that were never about that thread. The
expensive artifact is the embedding, which is not duplicated either way, and the
duplicated text is `RETENTION_SESSION`, so the pruner clears it within a day.

The pointer exists anyway, as metadata rather than as the storage model:
episodic records carry `session_ref`, so a recall hit can offer to read the
surrounding thread. It addresses the **thread, not the message** — ingest runs
before the transcript append, so the sequence number does not exist yet. Advisory
either way: a dead pointer means the link is stale, never that the memory is
wrong.

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

It is also a **full scan** of every session's every message — no index. That is
fine at one-session-per-live-chat scale, which is what a personal deployment is;
it is the first thing to reconsider if a deployment ever holds thousands of
threads.

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

#### Ownership and who may read a record

Every user-originated record carries an `owner` — a principal reference
(`user:alice`, or `chat:telegram:-100…` for something belonging to a conversation
rather than a person) — and a `visibility` of `PRIVATE` or `SHARED`. Reads take
a `memory.Audience`, and the type is the point:

```go
memory.For(principal)   // owned-by-them, plus shared, plus legacy
memory.Everyone()       // spelled out; three callers, each already holding the store
memory.Audience{}       // matches nothing
```

Search used to take a `scopeFilter string` where `""` meant everything, and both
production callers passed `""` — `memory_search`, and worse the `ContextEngine`,
which injects recalled memories into the system prompt **on every turn with no
tool call in front of it**. On a shared node that put one user's memories into
another's prompt before they had said anything. The fix is not that two callers
were careless: it is that the dangerous value was the easy one to write. The
zero `Audience` now matches nothing, so forgetting it yields empty results
rather than everyone's memories, and `TestNoUnscopedVectorSearch` parses the
tree and fails on a string in that argument position.

**An unowned record is readable by nobody** — only `Everyone()`, the operator
path, reaches it, so anything that does slip through can still be cleaned up.

There was briefly a carve-out making unowned readable by all, on the grounds
that an upgrade must not hide an existing node's entire memory. lobslaw has
never been deployed, so no records predate the owner field and that carve-out
guarded the empty set — a standing fail-open protecting a population that
cannot exist.

Nothing writes an empty owner either: every `types.Claims` construction in the
tree yields a user id — `anon` for unauthenticated REST, `webhook:<name>`,
`scheduler`, the Telegram identity. So an unowned record means a bug upstream,
and `EpisodicIngester.IngestTurn` refuses to write one rather than persisting
something nothing can recall.

An *owned* record with `UNSPECIFIED` visibility is likewise treated as private:
a writer that forgets the field must not silently publish.

`owner` is deliberately not `scope`. That field is a category (`episodic`,
`default`), and `Claims.Scope` is a permission tier — neither identifies a
person, and overloading either a third time is how the original bug hid.

#### Reading across owners

Someone has to be able to read across owners — an operator recovering a
deployment, or clearing up after a person who has left. Operator authority for
that is deliberately split in two. Machine operations (backup, restore, node
lifecycle) are authorised by an mTLS client certificate; access to a *person's*
data is authorised separately, by a principal holding `role:operator`, and a
certificate alone never grants the unrestricted audience.

The data half is what is built. `Claims.Roles` arrives either from a token's
`roles` claim or, for channels with no token, from `[[user]] roles` in operator
config resolved by principal. `TurnIdentity` carries it so a builtin can put the
turn back through the policy engine without holding the request's claims.

**Holding the role is not the grant.** Every widening goes through
`compute.CrossOwnerAuthorizer`, implemented over the policy engine as
`memory:read:any` on `memory:*`:

```go
readAudience(ctx, turn, authz)  // Everyone() only when policy says allow
```

The obvious implementation — `claims.HasRole("operator")` at the point of the
read — gives a deployment exactly two states: reads nothing of anyone else's, or
reads everything always and silently. An operator who wants their own access
narrowed to one owner, gated, or revoked for a week has nowhere to say so.
Routing it through a rule makes the widening something with a subject, an
effect, and a priority. `deny` and `require_confirmation` both refuse:
`ContextEngine.Assemble` and the `Forget` scope check have no user in front of
them to confirm with, and an effect an operator chose to slow something down
must not become the one that speeds it up.

**A nil authorizer never widens.** A deployment that has not wired one has not
said operators may read everything, and reading silence as universal read turns
an incomplete wiring into a breach that nothing in the logs distinguishes from
normal traffic.

**Every widening is an audit event.** The authorizer appends to the hash-chained
log — actor, rule id, effect — before returning true, which is why the policy
check and the audit write live in one implementation in `internal/node` rather
than at each reading call site. The complaint that motivated the operator role
was an audit trail the subject could write; a reader that forgets to log cannot
exist if logging is not the reader's job.

Applies at `memory_search` (both the semantic and substring strategies),
`ContextEngine.Assemble`, and `memory_forget` / `memory_correct`.

#### Consolidation and forgetting

Dream **never clusters across owners**. Consolidation replaces a cluster's
members with one summary carrying all their `SourceIds`, so a cross-owner
cluster would mint a record holding two people's memories and owned by neither
— and unowned reads as legacy, so everyone would see it. No read-side filter can
undo a merge after the fact. Two similar memories held by two people are not
duplicates; they are a coincidence. `applyMerge` re-checks and refuses rather
than trusting that guarantee, because the cost of it failing is unrecoverable.

A consolidation inherits its members' owner and the **most restrictive** of
their visibilities: a summary of anything private is private, or one shared
member in a cluster would publish the rest.

`Forget` takes a `requester` and will not delete what that principal cannot
read. The scoping happens *before* the `SourceIds` cascade, so a record filtered
out cannot pull its consolidations down with it. An empty requester is
unrestricted for operator tooling and peer nodes reaching the RPC over mTLS.

The agent's `memory_forget` sets a requester from the turn, so the model never
reaches the unrestricted path by default. A caller the policy engine has granted
`memory:read:any` is the exception: the RPC has one field for whose read this
is, and a caller excused from ownership filtering has nothing to put in it, so
the widened form is the empty requester. The principal is not lost by that — the
authorizer names it in the audit entry before the call is made.

### Where identity comes from

`TurnIdentity` travels on the request context, attached once per turn in
`Agent.runLoop`, and is the only source of caller identity for any builtin —
not just the session tools. It carries the user, their permission scope, the
roles they hold, the conversation address, and the timezone.

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

Identity is resolved through the operator's `[identity.aliases]` map before any
of this: `TurnIdentity.UserID` is what the channel called the caller
(`tg-@alice`, a REST subject) and is kept for audit, while `TurnIdentity.Principal`
is the canonical identity that ownership and visibility are decided against.
Authorising on the raw id makes one human several — they stop finding their own
history the moment they switch app — and authorising on nothing makes everyone
one person, which is the bug.

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
without one. Bare contexts come from operator tooling (`lobslaw session` reads
the store directly) and the compactor — callers that already hold the whole
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

---

## Pinned memory

The archive plus per-turn recall means a vector miss on "prefers terse
replies" makes the turn behave as though it were never said. Some facts
cannot be subject to a retrieval hit, so they go in the system prompt
every turn instead.

Two blocks, kept as separate records — a profile is about a person and
notes are about an environment, they are edited for different reasons,
and one overflowing must not squeeze the other:

| Block | Contents | Default cap |
|---|---|---|
| `profile` | who the user is | 1375 chars |
| `notes` | conventions, quirks, environment facts | 2200 chars |

Configurable as `memory.pinned_profile_chars` / `memory.pinned_notes_chars`.
**Characters, not tokens**: a character count is model-independent, and
a limit that moves when the tokeniser changes is not one an operator
can reason about.

### The cap is the feature

A write past it **errors** rather than truncating. Silently dropping
the tail would remove the pressure that forces curation *and* lose the
content. The error reports current usage so the model can decide what
to consolidate rather than guessing.

At 80% Dream is asked to propose a tidy-up asynchronously. The cap
creates the pressure; Dream does the work — which is where this
improves on hermes, who have no background consolidator and so force
the model to consolidate in the same turn.

### Frozen per session

The blocks sit in the part of the prompt a provider caches. Reading
them fresh each turn would mean every write invalidated the prefix for
the turn after it — always-on *and* never cached, the worst of both.

So writes are durable immediately and the **rendered snapshot** is
frozen for the session. That is hermes's trick, stated in their
`memory_tool.py`:

> Mid-session writes update files on disk immediately (durable) but do
> NOT change the system prompt — this preserves the prefix cache for
> the entire session.

A session boundary is not observable here (a Telegram conversation has
no end), so it is approximated by a 30-minute window. Snapshots are
keyed on conversation **and** user: in a group chat the prefix flips as
speakers alternate, which costs cache hits. The alternative would be
showing one participant's profile to everybody.

### Editing

By unique substring, not by id. An id-addressed store means the model
must read an index before it can change one line — a round trip to edit
a sentence. An ambiguous fragment is **refused rather than guessed at**:
editing the wrong memory is worse than being told to be more specific.

### Two guards worth knowing about

**promptguard on write.** These blocks are agent-written and land in
the most privileged position in the request. That the *store* is
trusted says nothing about the *content* — a fact learned from a
fetched page can carry an instruction, and pinning it would put that
instruction in system position on every future turn.

**A per-turn failure cap.** A hard cap plus "consolidate now" is a
livelock waiting to happen. hermes added
`_MAX_CONSOLIDATION_FAILURES_PER_TURN = 3` after a fragile replace/add
loop *"suppressed the user's reply"*. After three failures the tools
return a terminal **non-error** telling the model to stop and answer —
non-error because an error invites another attempt — and stop touching
the store entirely. A memory side effect must never cost somebody their
reply.

The user is taken from the turn identity, never from a parameter: a
user id the model can supply is one a prompt injection can supply, and
writing into somebody else's always-on block would put attacker text in
their system prompt forever.

---

## The self-taught store

Provenance by **location**, not by tag. If a record is in
`BucketSelfTaught`, the agent authored it — there is no marker to
forget, forge, or lose.

| Property | Why the separate store buys it |
|---|---|
| Provenance | structural, not remembered |
| Disable | a capability boundary, not a branch |
| Blast radius | "forget everything you taught yourself" is one operation |
| Audit | "show me what it decided on its own" is one namespace scan |
| Curator scope | the curator's domain *is* the store |

The boundary is behaviour: an episodic record is data the model *may*
retrieve and weigh; a skill or a pinned entry is an instruction it
*follows*. Those deserve different custody.

### The switch

```toml
[self_learning]
mode = "propose"   # "off" | "propose" | "auto"
```

Three states because "on/off" is the wrong shape here — it leaves no
room for *"write it down but do not act on it until I have looked"*,
which is the setting most people want and the default.

**`off` is enforced at wiring.** `wireSelfTaught` builds nothing and
every dependent is *absent*. "The capability is not present" is a
different and stronger claim than "the call sites check a flag", and
the second is not what an operator disabling this is asking for. An
unrecognised mode is off: a typo must never be the reason an agent
started following its own instructions.

The mode decides an artefact's initial state, **not the caller** — a
caller that could choose would eventually choose ACTIVE somewhere and
the operator's setting would stop meaning what it says.

### Archive, never delete

Deletion is not a lifecycle transition. For a product whose pitch is
trust, an agent that can silently erase evidence of what it taught
itself is the wrong default.

```
lobslaw learned list
lobslaw learned list --archived
lobslaw learned discard --apply       # archives everything unpinned
lobslaw learned restore <id> --apply  # comes back PROPOSED, not ACTIVE
```

Restoring returns something as a proposal because archiving implied a
decision, and putting it straight back in force skips it.

Pinned artefacts resist archiving and `discard` both: somebody who
decided an artefact is worth keeping should not have to defend it every
fortnight.

Archiving is two writes (the FSM routes by state, and a record cannot
be in two buckets atomically). A crash between them leaves a copy in
both; the live listing filters `ARCHIVED` out, so the duplicate is
invisible and the next archive clears it. The failure mode is a stale
copy nobody reads — the right side to err on for a store whose promise
is that nothing is lost.

### New, or a refinement?

Exact-name matching only catches a proposer that reuses the same
string. "tidy-notes" and "tidying-notes" are two artefacts doing one
job, and nothing downstream can reconcile them — the curator has no
basis to merge them, and the model sees two index entries contradicting
each other about how to do the same task.

So a near-duplicate search runs **before** anything new is accepted,
and the proposer has to say which it is:

```go
Propose(ctx, rec, ProposeIntent{Refines: "skill:tidy-notes", Rationale: "..."})
Propose(ctx, rec, ProposeIntent{Distinct: true})
```

With neither, a candidate scoring above `SimilarityThreshold` (0.72) is
**refused** — carrying the candidates in the error, so the proposer can
decide without a second round trip. Refuse rather than guess, the same
call the pinned-memory editor makes for an ambiguous match: picking
wrong produces a second instruction for one job.

Two signals. **Lexical always runs** — no dependency, cannot fail, and
catches the common case of a near-identical name. **Semantic runs when
an embedder is wired**, catching what lexical cannot ("tidy-notes"
against "organise-scratchpad"). The higher of the two wins rather than
an average: a pair that is lexically identical and semantically distant
is still a collision, and averaging would hide it.

A configured embedder that *errors* fails the propose rather than
silently degrading to lexical. The check exists to stop duplicates, and
quietly skipping half of it produces exactly what it was wired to
prevent.

Threshold is lower than the 0.88 used to cluster memories, because the
costs are asymmetric: a false positive costs one extra field on a call,
a false negative creates a permanent duplicate.

**Not auto-adjudicated**, deliberately. `Adjudicator` decides
merge/conflict/supersedes for *memories*, and reusing it here would
look consistent and be wrong — a memory is data the model may weigh, a
skill is an instruction it follows, and silently merging two
instructions produces a third nobody wrote.

### Refinements stage; they do not displace

A refinement lands as `record.Pending`. **The active version keeps
working.**

```
ACTIVE v1  ──propose refinement──>  ACTIVE v1 + pending
                                          │
                                    accept │ reject
                                          ▼
                                    ACTIVE v2   /   ACTIVE v1 unchanged
```

Without this, a skill used successfully for a month stops loading
because the agent had an idea about improving it. Approving swaps the
staged body in and bumps the version in one write; rejecting leaves the
live version byte-identical.

A rationale is **required** on a refinement — a diff with no reasoning
is one nobody can approve with any confidence.

An exact-name collision routes as a refinement whether or not the
proposer noticed, rather than overwriting.

In `auto` mode a refinement applies directly, because there is nothing
to wait for — that is what the operator asked for by choosing auto.

```
lobslaw learned pending
lobslaw learned accept <id> --apply
lobslaw learned reject <id> --apply
```

### Version history

Every superseded version is snapshotted before it is replaced, so a
refinement that turned out worse can be undone without the original
being rewritten from memory.

```
lobslaw learned history <id>
lobslaw learned rollback <id> <version> --apply
```

A rollback is itself a **new version**, not a reuse of the old number —
two different records at one version would stop the history being a
sequence anybody can reason about — and the version rolled away from is
snapshotted first, so rolling back to the wrong one has not destroyed
what you were on.

**`self_learning.history_depth`** (default 10) bounds it. Named for
what it bounds: `keep_versions` does not say whether the active version
counts, which is the first thing anybody asks. It counts **prior**
versions; the active one is always kept and does not count.

Bounded because it is not free — every version lives in the log and in
every snapshot thereafter, on every node. Unbounded history is a
store-growth problem that surfaces months later as slow snapshots.

History keys are zero-padded (`skill:tidy@00000007`) so a prefix scan
is version order. Without the padding v10 sorts between v1 and v2, and
"the oldest version" becomes whichever one looks smallest as a string.

### Size limits

Every raft apply replicates to every node and lives in snapshots
thereafter, so one oversized artefact bloats every node permanently.

| Limit | Default | Config |
|---|---|---|
| per file | 256 KiB | `self_learning.max_artefact_file_bytes` |
| per artefact | 1 MiB | `self_learning.max_artefact_total_bytes` |

Exceeding either **fails the write and names the offending path** —
rather than splitting, truncating, or accepting with a warning. The
author is the only party positioned to fix it, and "too large" without
saying which file leaves them guessing at a bundle they may not have
assembled by hand.

Total is checked separately from per-file, or a bundle of a hundred
just-under-limit files would pass.

The store holds instructions, not payloads. Anything genuinely large
belongs in storage, content-addressed, with only its digest travelling
in the log.

### Usage counters

A bucket, not a sidecar file, for reasons stronger than tidiness:
counters inside a manifest would invalidate its digest and break
verification; a per-node file is invisible to peers, so every node but
one would compute staleness from partial data; and the FSM already
provides the atomicity a sidecar needs `fcntl` locking for.

**Batched in-process and flushed**, never one `raft.Apply` per use.
Counter bumps are high-frequency and low-value, and paying consensus
for each is the obvious way to make this worse than the file it
replaces. Losing a handful of counts to a crash is acceptable; losing
the write path to contention is not.

---

## Staging what the agent writes

```toml
[memory]
write_approval = true    # off by default
```

Memory that silently self-modifies and cannot be inspected is a trust
problem. `lobslaw memory list` and the consolidation log answer *what
happened*; neither answers *may this happen*, and the write lands
either way.

With the flag on, every `memory_write` the agent makes is staged for
approval before it runs:

```
The agent wants to write a memory.
  remember: john prefers terse replies (tags ["preference"])

  [ once ]  [ this conversation ]  [ always ]  [ no ]
```

**Everything the agent writes**, not a guessed-at subset. A boundary
drawn around "facts about the user" has to be inferred, and inference
gets it wrong in both directions — missing the thing you cared about,
and asking about a working note you did not.

### It is a policy rule, not a branch

The flag installs a low-priority rule:

| | |
|---|---|
| action | `memory:write` |
| effect | `require_confirmation` |
| priority | lowest expressible |
| id | `config:memory.write_approval` |

That is what makes the answer reusable. **"This conversation"** becomes
a session grant; **"always"** mints a visible, revocable policy rule;
and an operator wanting something narrower writes an ordinary rule that
outranks the default. A branch consulting a boolean would have needed
its own notion of "already approved", and grown a second, subtly
different approval system beside the one R2 built.

The rule is held **in memory**, not written to the rule bucket. That
bucket is raft-replicated operator intent; a rule derived from one
node's config file is neither, and every node writing its own copy at
boot would turn a local setting into contested cluster state.

### Its own action

`memory:write`, not `tool:exec`.

Every deployment allows `tool:exec` on `memory_write` — otherwise the
tool could not run at all — so reusing that action would mean the allow
rule already in place silently satisfied the gate. `RequireApproval`
takes no action parameter, so passing the wrong one is not expressible.

### The prompt carries the content

Unlike a trace span, which carries no content ever, the confirmation
shows what is about to be written. The audience is the difference: a
span goes to whatever telemetry the operator runs, while this goes to
the person being asked to decide. A prompt that withholds the content
cannot be answered on its merits, so it gets answered reflexively —
which is worse than not asking.

Truncated at 200 characters, because a confirmation three screens long
is one nobody reads to the end.

A **denial** carries no content. It is not a question, and the person
seeing it is not deciding anything.
