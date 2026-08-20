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

| requirement | why |
|---|---|
| **WordPiece** or **SentencePiece Unigram** tokenizer | The two that are implemented — BERT family and XLM-RoBERTa family respectively. |
| **`safetensors`, F32 weights** | A `pytorch_model.bin` is a Python pickle: loading one executes arbitrary code, and a model arrives over the network from a host named in config. F16/BF16 are not read yet. |
| **`gelu`** activation, absolute positions | The exact erf form, as `hidden_act = "gelu"` means in HuggingFace. RoPE-based architectures are a different forward pass. |

A checkpoint failing any of these is rejected at boot with the reason, not at first recall.

#### Which model to pick

**The question is whether you need more than one language.**

```toml
# English only — the default choice, and the best one for English.
# Mirrored in this project's releases, so nothing is fetched from a
# third party and the bytes are pinned to a tag.
model        = "all-MiniLM-L6-v2"
download_url = "https://github.com/jmylchreest/lobslaw/releases/download/models-all-MiniLM-L6-v2"

# Multilingual — from HuggingFace, as it is too large to mirror.
model        = "multilingual-e5-small"
download_url = "https://huggingface.co/intfloat/multilingual-e5-small/resolve/main"
```

Measured on 20 paraphrase queries against 20 stored memories — the shape of the actual workload:

| model | download | languages | recall@1 | recall@3 | margin |
|---|---|---|---|---|---|
| `all-MiniLM-L6-v2` | **91 MB** | English | **16/20** | **20/20** | **+0.193** |
| `multilingual-e5-small` | 471 MB | 100+ | 15/20 | 18/20 | +0.018 |
| `multilingual-e5-base` | 1.1 GB | 100+ | 15/20 | 19/20 | +0.019 |
| `intfloat/multilingual-e5-large` | 2.2 GB | 100+ | untested | | |
| `Shitao/bge-m3` | 2.3 GB | 100+, 8k ctx | **tested** | best separation, ~7x slower | |

Twenty queries is a small sample, so read these as directional. Two things are clear anyway:

**On English, the 91 MB model wins.** Perfect recall@3 at a fifth the size. Multilingual capability is not free — it costs both download and accuracy in any single language.

**The margin column matters more than it looks.** e5 compresses everything into 0.79–0.90, so a similarity threshold sits in noise and has to be tuned against real data. MiniLM separates properly, which makes thresholds mean something.

Going multilingual buys retrieval across languages, and that is real: with e5, a memory recorded in English is found by a question asked in Spanish. MiniLM cannot do that — a French query scores 0.11 against an English memory, below unrelated English records at 0.14, so it would simply never be retrieved.

#### Why the multilingual models are so much larger

The vocabulary, not the model:

```
multilingual-e5-small       471 MB total
  embedding table           384 MB   (250,037 tokens x 384 dims x 4 bytes)  = 82%
  the actual transformer     87 MB

all-MiniLM-L6-v2             91 MB total
  embedding table            47 MB   (30,522 tokens x 384 dims x 4 bytes)
  the actual transformer     44 MB
```

Both are 384-dimensional. The difference is one row per token for a vocabulary covering 100+ languages.

The 512-token context is less limiting than it looks: longer text is **chunked automatically** and combined by a length-weighted mean, so `bge-m3`'s 8k window mainly saves you the chunking.

#### Choosing between models

Changing the model is close to a one-way door — the node refuses to boot until the whole corpus is re-embedded — so measure first, against your own memories rather than a published benchmark:

```
lobslaw embed-eval --config config.toml all-MiniLM-L6-v2 multilingual-e5-small
```

```
30 records from data/state.db (27 skipped: no distinct event/context, or unreadable)

model                                dims  recall@1  recall@3    margin    per doc
all-MiniLM-L6-v2                      384       70%       87%   +0.0893      77.2ms
multilingual-e5-small                 384       67%       87%   +0.0176     136.1ms
```

The node must be **stopped** — bbolt takes an exclusive lock.

No question set is needed. Each record carries an `event` (a short summary) and a `context` (the fuller text); the event becomes the query and the context the document, so a model that understands the record ranks that record's own context first. The ground truth is the pairing already in the store. Records whose event and context are identical are skipped, since retrieving those is free for any model.

`recall@3` is the number to weigh most — the context engine puts several records in front of the model, not one. `margin` is how far ahead the right answer sits: good recall with a thin margin means any similarity threshold you set is a coin toss.

#### Where to fetch each model

Copy the `download_url` for the model you want. Every URL below was checked: all of them serve `config.json`, `model.safetensors`, `tokenizer.json` and `1_Pooling/config.json`.

**`all-MiniLM-L6-v2`** — English, 91 MB, the default. Mirrored here, so nothing is fetched from a third party:

```toml
model        = "all-MiniLM-L6-v2"
download_url = "https://github.com/jmylchreest/lobslaw/releases/download/models-all-MiniLM-L6-v2"
```

**Multilingual** — from HuggingFace, which is where they live:

```toml
model        = "multilingual-e5-small"    # 471 MB
download_url = "https://huggingface.co/intfloat/multilingual-e5-small/resolve/main"

model        = "multilingual-e5-base"     # 1.1 GB
download_url = "https://huggingface.co/intfloat/multilingual-e5-base/resolve/main"

model        = "multilingual-e5-large"    # 2.2 GB
download_url = "https://huggingface.co/intfloat/multilingual-e5-large/resolve/main"

model        = "bge-m3"                   # 2.3 GB, 8k context
download_url = "https://huggingface.co/Shitao/bge-m3/resolve/main"
```

The pattern for any other XLM-RoBERTa or BERT checkpoint is `https://huggingface.co/<org>/<repo>/resolve/main`. `model` is a directory name under `<data_dir>/models` and need not match the repository.

Only MiniLM is mirrored. `multilingual-e5-large` and `bge-m3` exceed a GitHub release asset's 2 GB per-file limit, so mirroring them would need splitting and reassembly; `e5-small` would fit but has not been done. All are MIT, so anyone can mirror them — see the release for the layout, which is an ordinary snapshot plus `SHA256SUMS` and one rename (`1_Pooling/config.json` becomes `1_Pooling.config.json`, because an asset filename cannot contain `/`).

#### Verification

A mirror that publishes `SHA256SUMS` is checked against it, and a mismatch is fatal — the file is deleted so the next boot re-downloads rather than loading bytes already known to be wrong. HuggingFace publishes no such manifest, so upstream downloads are unverified; that is the trade for being able to use them at all.

To pin a model completely, mirror it yourself and publish a manifest alongside.

#### What does not work

- **`BAAI/bge-m3` itself** ships only `pytorch_model.bin`, no safetensors, so it is refused. `Shitao/bge-m3` — the author's own repository — has safetensors and is otherwise identical, but its licence is undeclared where BAAI's original is MIT.
- **`gte-multilingual-base`** is a different architecture (`NewModel`, RoPE) and ships F16.

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
lobslaw run --allow-embedding-model-change     # once, to get the node up
lobslaw memory reembed                          # repair
lobslaw run                                     # normally from here
```

The escape hatch is needed because the guard refuses at **boot** and the repair needs a **running** node. It is a command-line flag rather than a config key so nobody can leave it on by accident, and it logs a warning for as long as it applies: recall is genuinely wrong until the re-embed finishes, since queries are scored against another model's vectors.

`reembed` runs on the **live** node and proposes every write through raft, so the change survives a restart. It leaves dream's consolidations alone — those embed several records at once and dream rewrites them on its own schedule. The offline `backfill-embeddings` tool writes `state.db` directly and the node rebuilds from its log on boot — its writes survive, its deletions do not, so a model change never fully lands. Use it only for a cluster that will not start at all.

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
