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

#### What a model must be

Three requirements, each a refusal rather than a preference:

| requirement | why |
|---|---|
| **SentencePiece Unigram** tokenizer | The only tokenizer implemented. In practice this means the **XLM-RoBERTa** family. |
| **`safetensors`, F32 weights** | A `pytorch_model.bin` is a Python pickle — loading one executes arbitrary code, and a model arrives over the network from a host named in config. F16/BF16 are simply not read yet. |
| **`gelu`** activation, absolute positions | The exact erf form, as `hidden_act = "gelu"` means in HuggingFace. RoPE-based architectures are a different forward pass. |

A checkpoint failing any of these is rejected at boot with the reason, not at first recall.

#### Which model to pick

**Start with `intfloat/multilingual-e5-small`.** It is the smallest thing that works, and for most people it is enough.

| model | download | context | licence | status |
|---|---|---|---|---|
| `intfloat/multilingual-e5-small` | **471 MB** | 512 | MIT | tested, used by CI |
| `intfloat/multilingual-e5-base` | 1.1 GB | 512 | MIT | tested |
| `intfloat/multilingual-e5-large` | 2.2 GB | 512 | MIT | compatible, untested |
| `sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2` | 471 MB | 512 | Apache-2.0 | compatible, untested |
| `Shitao/bge-m3` | 2.3 GB | **8192** | undeclared | compatible, untested |

All are multilingual — a memory recorded in English is retrievable by a question asked in Spanish.

The 512-token context is less limiting than it looks: longer text is **chunked automatically** and combined by a length-weighted mean, so `bge-m3`'s 8k window mainly saves you the chunking, not the content.

#### Is the small one enough?

For memory recall, yes. Measured on 20 paraphrase queries against 20 stored memories — the shape of the actual workload:

| model | download | recall@1 | recall@3 |
|---|---|---|---|
| `multilingual-e5-small` | 471 MB | 15/20 | 18/20 |
| `multilingual-e5-base` | 1.1 GB | 15/20 | 19/20 |

Identical top-1, one extra hit in the top 3, for 2.4x the download. Treat that as "no meaningful difference" rather than a precise result — 20 queries is a small sample — but the direction is clear enough that the larger model is hard to justify for this.

That is what you would expect from the task: a few thousand short records, queries that are paraphrases of them, and the top few going into a prompt. It is an easy retrieval problem, and recall@3 is what matters because the context engine takes several.

#### Why 471 MB is the floor

Because the vocabulary is the cost of being multilingual, not the model:

```
multilingual-e5-small       471 MB total
  embedding table           384 MB   (250,037 tokens x 384 dims x 4 bytes)  = 82%
  the actual transformer     87 MB
```

Twelve layers of transformer are under 90 MB. The other 82% is one row per token for a vocabulary covering 100+ languages. Nothing in the XLM-RoBERTa family escapes that, so there is no meaningfully smaller multilingual option.

An English-only embedder would be ~90–130 MB, five times smaller — but every one of them (`bge-small-en-v1.5`, `all-MiniLM-L6-v2`, `e5-base-v2`) is **WordPiece**, which the tokenizer does not read.

#### What does not work

- **WordPiece models** — the forward pass would run them; the tokenizer is Unigram-only.
- **`BAAI/bge-m3` itself** ships only `pytorch_model.bin`, no safetensors, so it is refused. `Shitao/bge-m3` — the author's own repository — has safetensors and is otherwise identical, but its licence is undeclared where BAAI's original is MIT. Check before relying on it.
- **`gte-multilingual-base`** is a different architecture (`NewModel`, RoPE) and ships F16.
- **`distiluse-base-multilingual-cased-v2`** is DistilBERT with a WordPiece tokenizer.

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
