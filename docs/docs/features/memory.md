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

For "find similar to X" queries, episodic records are also embedded and indexed. Without an embedder this degrades to lexical matching rather than failing — which works, but cannot match a paraphrase: *"what do I use for config?"* will not find *"prefers TOML"*.

There are two ways to provide one.

### Built-in (no API key, no egress)

A model this node runs in-process. Embeddings are computed for **every** record, including private ones, so a remote embedder is a standing disclosure of the whole corpus to a third party — this avoids that entirely.

```toml
[compute.embeddings]
type         = "builtin"
model        = "multilingual-e5-base"
download_url = "https://huggingface.co/intfloat/multilingual-e5-base/resolve/main"
```

The model is cached under `<data_dir>/models/<model>/` and downloaded on first boot if absent. **There is no default URL**: leave `download_url` empty and nothing is fetched — a missing model becomes an error at boot, which is what an air-gapped node wants. The host is granted egress under the `embedding-model` role only when the URL is set.

#### Which models work

A checkpoint must be **XLM-RoBERTa family** (SentencePiece **Unigram** tokenizer), `gelu` activation, and **`safetensors` with F32 weights**. Verified:

| model | size | context | licence |
|---|---|---|---|
| `intfloat/multilingual-e5-small` | 466 MB | 512 | MIT |
| `intfloat/multilingual-e5-base` | 1.1 GB | 512 | MIT |
| `intfloat/multilingual-e5-large` | 2.2 GB | 512 | MIT |
| `Shitao/bge-m3` | 2.3 GB | **8192** | see note |

All are multilingual, so a memory recorded in one language is retrievable by a question asked in another.

**What does not work, and why:**

- **WordPiece models** — `bge-small-en-v1.5`, `all-MiniLM-L6-v2`, `e5-base-v2`, `LaBSE`. The forward pass would run them; the tokenizer is Unigram-only.
- **`BAAI/bge-m3` itself** ships only `pytorch_model.bin`, with no safetensors. A pickle is arbitrary code execution on load, so it is refused. `Shitao/bge-m3` — the author's own repository — has safetensors and is otherwise identical. Its licence is not declared on the repo, unlike BAAI's MIT original, so check before relying on it.
- **`gte-multilingual-base`** is a different architecture (`NewModel`, RoPE-based) and ships F16 weights.

Start with `multilingual-e5-base` unless you need more than 512 tokens per memory in one piece — longer text is chunked automatically, so the 8k context matters less than it looks.

`model` is also the identity stamped on every vector, so changing it is refused at boot until the corpus is re-embedded — see below.

### Remote

```toml
[compute.embeddings]
type        = "remote"   # the default
endpoint    = "https://api.openai.com/v1/embeddings"
api_key_ref = "env:OPENAI_API_KEY"
model       = "text-embedding-3-small"
dims        = 1536
```

`dims` must match the model's actual output width. Note that **OpenRouter serves no embeddings** — it proxies chat models only.

### Changing the embedding model

Vectors from two different models are not comparable, and at the same width nothing detects it: cosine still returns a number, the number still sorts, and every search is confidently wrong. So each vector records which model wrote it, and a node whose configured model disagrees with its corpus **refuses to start**, naming both and the way out:

```
go run ./cmd/backfill-embeddings --force
```

### Querying

Via the `memory_search` builtin, which prefers semantic search when an embedder is configured and falls back to lexical when one is not — including when a configured embedder fails, so an outage degrades rather than breaks.

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
