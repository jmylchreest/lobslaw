---
sidebar_position: 8
---

# Memory

Episodic + semantic + soul. The persistent context the agent draws on.

## Three layers

| Layer | Purpose | Storage |
|---|---|---|
| **Episodic** | What happened in conversations — turn-by-turn record | bbolt + raft replication |
| **Semantic** | Embedding-indexed for similarity search | bbolt; embeddings via `[compute.embeddings]` |
| **Soul** | Operator-curated agent persona — tone, preferences, persistent traits | `SOUL.md` + raft-replicated tunables |

All three are queried per-turn to assemble context.

## Episodic

Every conversational turn writes an `EpisodicRecord`:

```protobuf
message EpisodicRecord {
  string  id          = 1;
  string  user_id     = 2;
  string  channel     = 3;
  string  event       = 4;     // "user_turn", "agent_turn", "tool_call_done"
  string  context     = 5;     // human-readable
  google.protobuf.Timestamp ts = 6;
  string  turn_id     = 7;
}
```

Plus payload-specific fields. Recall uses `event + ts + user_id` to retrieve the last N turns or matches a date range.

The agent's per-turn prompt includes recent episodic records by default — typically the last 5-10 turns. This is what makes the agent feel like it remembers a conversation.

## Semantic

For "find similar to X" queries, episodic records are also embedded and indexed. The embedder is configured under `[compute.embeddings]`:

```toml
[compute.embeddings]
endpoint    = "https://openrouter.ai/api/v1/embeddings"
api_key_ref = "env:OPENROUTER_API_KEY"
model       = "openai/text-embedding-3-small"
dimensions  = 1536
```

Query via the `memory_recall` builtin:

```
memory_recall(query="when did we talk about deploying to railway?", k=5)
```

Returns the top-k episodic records by cosine similarity. Useful for "find that thing you said about X" — where exact-match search would fail.

## Soul

`SOUL.md` is operator-authored markdown describing the agent's persona, preferences, mannerisms. Loaded at boot, included in every system prompt.

```markdown
# Persona

I'm a no-nonsense assistant. I don't pad answers. I tell you when I don't know.

## Preferences
- Use UK spelling.
- Prefer terse over verbose.
- ...
```

In addition, **soul fragments** are short raft-replicated tunables — name + value pairs the operator can adjust without redeploying:

> **You:** soul_tune name="energy" value="conserve"
>
> **Bot:** Updated soul fragment "energy" → "conserve". Future turns will reflect this.

Fragments are merged into the system prompt under a `[fragments]` section. The operator can `soul_tune`, `soul_list`, `soul_history` to manage them.

## Dreams

Periodic background synthesis. The dream loop runs every `[soul] dream_interval` (default 24h) and:

1. Surveys recent episodic records (last 24h).
2. Asks the LLM to find patterns, recurring themes, gaps.
3. Writes a `DreamRecord` summarising — embedded, retrievable.

The next morning's first turn includes the dream as context. Effectively: "you noticed X and Y yesterday — keep an eye on it."

## Adjudication

When two episodic records conflict ("I told you I'm vegetarian" / "let's get sushi"), the adjudicator can be invoked to resolve:

```
memory_adjudicate(claim_a="...", claim_b="...")
```

Returns a resolution and writes a new record. Used sparingly; most "conflicts" are context-dependent and don't need explicit resolution.

## Forgetting

```
memory_forget(query="...")
```

Issues a soft-delete on matching records. Records are tombstoned (not physically deleted) so audit/rollback works.

## Recall heuristics

The context engine (`internal/compute/context_engine.go`) decides per-turn what to include:

- Recent episodic (last N turns).
- Top-k semantic recall on the user's current message.
- Active soul fragments.
- Recent dream (if present).
- Active commitments + scheduled tasks (so the agent knows what it's already promised).

Total context budget is bounded by `[compute.limits] max_context_tokens`; recall is truncated to fit.

## Reference

- `internal/memory/store.go` — bbolt + atomic.Pointer
- `internal/memory/dream.go` — dream synthesis
- `internal/memory/cluster.go` — semantic clustering
- `internal/compute/embedding.go` — embedder
- `internal/compute/context_engine.go` — per-turn assembly
- `internal/compute/builtin_memory.go` — agent-facing recall / forget / adjudicate
- `pkg/proto/lobslaw/v1/lobslaw.proto` — record schemas
