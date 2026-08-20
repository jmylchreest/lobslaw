# Deferred Work

Items consciously deferred past MVP. Each has a short note on why it's deferred and what would trigger revisiting it. Items marked LANDED have been removed — git history is authoritative.

## Modality routing

### Self-hosted modality sidecars (Parakeet / faster-whisper / LLaVA / ollama-vision)

The capability-driven router already works with self-hosted endpoints — point a `[[compute.providers]]` entry at `http://parakeet:8000/v1/audio/transcriptions` (or wherever) with `capabilities = ["audio-transcription"]` and `read_audio` picks it up. No code change needed.

**What's missing:** operator examples — a `deploy/docker/sidecars.yml` that bundles faster-whisper-server + ollama-vision so users have a working "no remote API" deployment to copy.

**Trigger to revisit:** Any time someone asks "how do I run this fully offline?" — the answer is the docker-compose example.

---

### Modality fallback chain at runtime

`SelectByCapability` returns providers in priority order, but the modality builtins currently register against only the first match. If the primary provider returns 5xx or rate-limits, the call fails — there's no automatic retry against the next-priority match.

**Why deferred:** Single-provider works fine when only one is configured (the common case today). Adding fallback requires deciding retry policy (5xx only? include 429? exponential backoff between providers?) — premature without operational data.

**Trigger to revisit:** First time the user reports "the bot couldn't read my image, MiniMax was down" while OpenRouter or Anthropic vision was also configured.

**How:** Pass the full ordered match list into `RegisterVisionBuiltin` / `RegisterAudioBuiltin` etc.; on transient HTTP failure, try the next entry. Cap retries at len(matches). Log per-attempt failures so operators can see degradation patterns.

---

### Pricing auto-pull from models.dev catalog

The catalog has `cost.input` / `cost.output` for most models; the `applyModelsDevAutoCapabilities` path could plumb pricing into `internal/compute/pricing.go` to replace the hardcoded table.

**Trigger to revisit:** First wrong budget calculation from price drift.

---

## Storage

### Cross-cluster storage tunneling / routing

A storage-enabled node materialises its own mounts locally. A compute-only node (no `storage` function enabled) cannot currently access mounts materialised on another node — it would need to ask a peer to proxy reads/writes.

**Why deferred:** Complex (proxying FUSE operations over gRPC while preserving POSIX semantics), and the MVP deployment pattern is storage-everywhere (every node enables the storage function).

**Trigger to revisit:** First deployment where an operator wants a pure compute-only node that still needs filesystem paths under `/cluster/store/...` without local mounts.

**How:** Add a `StorageService.Read` / `StorageService.Write` pair that proxies filesystem operations, plus a client-side FUSE mount on the compute-only node that calls those RPCs. ~2-3 weeks of careful work.

---

### Strict-security CA bootstrap (sign-RPC mode)

MVP treats CA material as an infrastructure concern — every storage-enabled node's initContainer has the CA private key and self-signs. For tighter security, the CA key could live on one node running a `ClusterBootstrap.Join` gRPC; other nodes present a CSR + HMAC proof from a shared join secret.

**Trigger to revisit:** First deployment where the CA private key must not leave a specific node.

---

## Cluster Membership

### `bootstrap_expect`-style cold-start coordination

Today every node decides per-node whether to bootstrap or join based on whether it can find a peer. Sequential bringup works. Simultaneous cold start of N empty-state nodes split-brains: each falls back to solo bootstrap, you end up with N independent single-voter clusters.

**Why deferred:** The simultaneous-cold-start case only arises in fully automated provisioning (k8s StatefulSet rollout, Terraform apply). Personal-cluster + container-compose deployments are sequential by nature.

**Trigger to revisit:** First fleet-style deployment.

**Workaround:** Bring up node-1 first, wait for `raft leadership changed is_leader=true`, then start the rest. Compose's `depends_on` handles this for the docker stack.

---

## MCP

### `.mcp.json` discovery from plugin/skill subdirs — PARTIAL

Operator-trusted config-directory discovery is wired. Auto-loading from plugin/skill subdirectories needs a trust story (signing/allowlist) before it's safe.

**Trigger to revisit:** First plugin-style integration that ships its own `.mcp.json`.

---

### Agent-driven MCP install (policy-gated)

Lobslaw could expose an `install_mcp` builtin that lets the agent install an MCP server at runtime via chat. The current `[[mcp.servers]] install = [...]` field is operator-only.

**Why deferred + why it stays deferred:** Letting an LLM install arbitrary code is a remote-code-execution surface dressed up as a feature. The OpenCode / Claude Code approach is right: install is operator-curated, agent uses what's available.

**Trigger to revisit:** A specific deployment where an operator explicitly wants an agent to manage its own toolchain — e.g. a multi-tenant dev sandbox. Default disabled, policy-gated, sandboxed install command.

---

### REST + webhook attachment passthrough

Telegram channel populates `IncomingMessage.Attachments`; REST + webhook channels skip the abstraction and go straight to `compute.ProcessMessageRequest{Message: text}`. Provider-native vision passthrough (multimodal main model rather than via `read_image` tool hop) is also not done.

**Trigger to revisit:** When the user wants attachments on REST/webhook, OR wants to swap to a vision-capable main model and bypass the read_image hop.

---

## Sandbox

Phase 4.5.5 hardening (NoNewPrivs + Landlock + seccomp BPF) and Phase E.5 (nftables) have landed. Two install layers remain.

### cgroup v2 install (CPU / memory limits)

Policy carries `CPUQuota` (millicpus) and `MemoryLimitMB`. Validate rejects negatives. Apply does not create or attach to a cgroup.

**Why deferred:** Cgroup v2 needs a delegated cgroup (systemd session, user slice), writing `cpu.max` / `memory.max`, and placing the subprocess's pid into `cgroup.procs` before exec. Moving parts are fine on bare Linux but flaky across WSL / older distros / rootless podman.

**Trigger to revisit:** First user report of a tool runaway. The `WaitDelay` kill path already bounds wall-clock.

**How:** `internal/sandbox/cgroupv2/install.go` — detect `/sys/fs/cgroup/cgroup.controllers`, create a leaf cgroup under our slice, write limits, fork-exec tool with its pid written into `cgroup.procs`. Gate via runtime-detect; no-op with log warning where cgroup v2 isn't available.

---

### Sidecar skill hot-reload

MVP hot-reloads normal skills. Sidecar-bearing skills require sidecar restart.

**Why deferred:** Sidecar lifecycle is a separate process; coordinating live restart without dropping in-flight tool calls is non-trivial.

**Trigger to revisit:** First user report that sidecar-reload interrupts their workflow.

---

## Operational

### Vector library upgrade (HNSW / FAISS)

MVP uses pure-Go cosine similarity over float32 slices. Adequate for memory < ~1M records.

**Trigger to revisit:** Memory store exceeds ~500k records and recall latency becomes noticeable.

---

### LLM prompt caching for Anthropic

MVP uses OpenAI-compatible API only. Anthropic's native SDK supports prompt caching (3–10× cost/latency win for repeated system prompts).

**Trigger to revisit:** First measurable LLM cost pain, or first serious bench of Anthropic as primary provider.

---

### LLM cost-table refresh automation

MVP ships hardcoded pricing defaults. Auto-refresh from provider pricing APIs is nice-to-have.

**Trigger to revisit:** First wrong budget calculation from price changes. Probably folds into the models.dev pricing pull above.

---

### slog-logfilter maintenance fork

`github.com/jmylchreest/slog-logfilter` is maintained by the same person running this project, so day-to-day "is it maintained?" is yes. If upstream goes dormant or needs unblockable changes, vendor or fork in-tree (`internal/logging/filter/`). Public API is narrow; the integration is ~70 LOC.

**Trigger to revisit:** Upstream goes 6+ months without a commit AND we need a change, OR a bug we can't patch upstream.

---

### `storage:<label>[/<path>]` reference scheme — exact semantics

References like `memory.snapshot.target = "storage:r2-backup"` work today as `<mount-path>/<path>`. Open questions:

- Are writes to the target gated by the mount's `read_only_paths` config?
- Should `storage:` references support cross-mount fallback?

**Trigger to revisit:** When the second consumer of the scheme lands and the answer to one of those questions matters.

---

## Infrastructure / Workflow

### Verify SHA pins in `.github/workflows/*.yml`

Workflow SHAs are pinned but were recalled from training data, not verified against published tags.

**Trigger to revisit:** First CI run that reports a SHA mismatch, OR routine dependency review (renovate config with `pinDigests: true`).

---

### Branch protection on `origin`

Required checks + PR workflow. Solo-work today; direct-to-main is fine.

**Trigger to revisit:** First external contributor, or first public release.

---

## Dev Environment Notes

### `get-key` alias

The user's OpenRouter API key is accessed via the zsh alias `get-key OPENROUTER_API_KEY_LOBSLAW`. Local-dev convenience, not a lobslaw concern — lobslaw reads `env:OPENROUTER_API_KEY_LOBSLAW` at runtime. Workflow: `OPENROUTER_API_KEY_LOBSLAW=$(get-key OPENROUTER_API_KEY_LOBSLAW) ./lobslaw ...`.

---

## Built-in embedder (`internal/embedder`)

A pure-Go XLM-RoBERTa encoder so a node can embed its own memories with no API key, no endpoint and no egress at query time. Loads BERT / RoBERTa / XLM-RoBERTa, which is one code path for bge-small (English), multilingual-e5-base and bge-m3.

**The feature is complete end to end.** A node with `type = "builtin"` does semantic recall with no API key, no endpoint and nothing leaving the machine: the SentencePiece Unigram tokenizer matches the reference on 119/119 cases, and a memory recorded in English is retrieved by the same question asked in French, German, Chinese or Japanese.

Landed: forward pass gated on measured tolerance; packed-GEMM kernel for AVX2; mmap'd weights (1.1 GB of heap becomes 0); `EmbedBatch`; chunked long input; tested goroutine safety; the tokenizer; `EmbeddingProvider` wired at boot.

What follows is refinement, not completion.

### Measured, on the default model

Numbers below are `all-MiniLM-L6-v2`, which is what the docs now recommend. Earlier entries here quoted `multilingual-e5-base` from before the packed GEMM and mmap landed, and were badly out of date.

Head to head against the reference implementation, identical text, end to end:

| text | reference | ours (AVX2) | ours (portable) |
|---|---|---|---|
| 40 chars | 5.7 ms | 8.1 ms (1.4x) | 13.7 ms (2.4x) |
| 204 chars | 23.6 ms | 30.8 ms (1.3x) | 63.9 ms (2.7x) |
| 819 chars | 70.5 ms | 133.3 ms (1.9x) | 260.6 ms (3.7x) |

So the vector kernel is within 1.3–1.9x of the reference, not the ~5x an earlier entry claimed. Allocations are 2,800–4,500 per encode, not the ~7,900 measured on the larger model.

Backfill throughput is linear with a flat heap — 10,000 records is roughly 15 minutes on the vector build. `TestBackfillScaling` re-measures it.

---

### Attention: parallel over heads, still on the dot kernel

The head loop was serial and each of its two matmuls spawned workers — 288 matmul calls per encode, every one paying goroutine setup for a problem too small to need it. Now the HEADS run in parallel and the matmuls inside them are serial, which is the natural unit: heads are independent by definition, there are about as many as a machine can use, and each is big enough to be worth a goroutine.

At 256 tokens, AVX2: 176 ms to 141 ms, and allocations from 4,466 to 991. The portable path gained too (340 ms to 293 ms).

Still on the dot kernel rather than the packed GEMM, because attention's operands are computed per call. Packing them is worth it at length — at 256 tokens packing K costs 16K writes against 4.2M MACs — but it needs its own scratch and has not been done.

**Trigger to revisit:** long-document embedding, where attention's share grows quadratically.

---

### Allocations per encode

508 at 16 tokens, 991 at 256 — down from ~2,800 and ~4,466, almost entirely by removing the per-matmul goroutines above.

The remaining ones are the per-call scratch in `hiddenStates`, which cannot become a `Model` field: `TestConcurrentEmbedIsSafeAndDeterministic` catches exactly that. A `sync.Pool` would work.

Note the trade already taken: per-head scratch raised bytes-per-op at 256 tokens from 4.6 MB to 8.7 MB, because each head needs its own L x L score buffer. Worth it for 4.5x fewer allocations and 20% less time, but it is a real cost at long sequence lengths.

---

### Portable kernel — as far as Go goes without assembly

arm64, and amd64 without `GOEXPERIMENT=simd`, take this path. Now 2.2x behind the vector one at 16 tokens and 2.0x at 256, from 2.4–3.7x.

Everything available in plain Go has been tried and measured:

- **8 accumulators instead of 4** — worth ~7%, kept. The count is bounded by how many partial sums the compiler keeps in registers.
- **Blocked tiles**, 4x4 through 4x32 — all SLOWER than the dot loop. Without vector types Go spills tile accumulators to stack and every FMA becomes two memory operations.
- **Row blocking to keep B in L1** — no measurable change.
- **Parallel over heads** — helped here too (340 ms to 278 ms at 256 tokens).

What remains is a Go assembly microkernel: NEON for arm64, AVX2 for amd64 without the experiment, with the portable loop as the fallback. That is real work and needs a fallback anyway, so the gap is a floor rather than an oversight.

**Trigger to revisit:** first arm64 deployment where embedding latency actually matters.

---

### e5 asymmetric prefixes — implemented, benefit unproven

`query_prefix` and `passage_prefix` are now configurable and applied on the right sides: `EmbeddingProvider` gained `EmbedQuery`, and the two query call sites (context engine, `memory_search`) use it while everything else embeds documents.

**The measured benefit did not survive contact with a real corpus.** A synthetic twenty-query set showed one extra hit at both recall@1 and recall@3. Re-measured with `embed-eval` on 30 real records, `multilingual-e5-small` scores 68% / 87% with the prefixes and 68% / 87% without — and the margin is slightly *narrower* with them (+0.0145 against +0.0169).

So it is available for anyone who wants it, correctly wired, and off by default. Do not expect it to help; the earlier claim that it was worth a hit at each rank was measured on a set small and curated enough to be noise.

Empty by default is also *correct* for the recommended model — `all-MiniLM-L6-v2` is symmetric, and prefixing it makes retrieval worse.

---

### Model size and mirroring — decided against, documented instead

Two related items closed by decision rather than by work.

**int8 quantisation of the embedding table** would take a multilingual model from 471 MB to roughly 180 MB, since 82% of it is one row per token for a 250k vocabulary. Not done: shrinking the *download* means producing and hosting a quantised artefact, which is a build pipeline and a hosting commitment rather than a code change. The recommended default is 91 MB, so this only ever mattered to multilingual deployments.

**Mirroring the multilingual models** is likewise not done. `multilingual-e5-large` (2.2 GB) and `bge-m3` (2.3 GB) exceed a GitHub release asset's 2 GB per-file limit outright and would need splitting with a reassembly step; `multilingual-e5-small` (471 MB) would fit. All are MIT, so it is permitted — it is the plumbing and the hosting nobody has signed up for.

Instead, `docs/docs/features/memory.md` lists a verified `download_url` for every supported model, and the release describes the mirror layout for anyone who wants to host their own. That is the part that actually blocked people: not the absence of a mirror, but having to work out the URL shape.

**Trigger to revisit:** a deployment that must be multilingual AND cannot reach HuggingFace.

---

### bge-m3 — tested and gated

`Shitao/bge-m3` (the author's own repository; `BAAI/bge-m3` ships only a pickle and is refused) loads and passes:

- 1024 dims, 8192 context, 250,002 vocab; loads in 876 ms
- Golden parity on **both** kernels, 119/119 tokenizer cases exact
- Cross-lingual separation far better than e5-small: an English memory against its French translation scores 0.87 and its Chinese 0.76, with an unrelated English record at **0.40** — where e5-small puts unrelated text at 0.79, leaving almost no room for a threshold
- ~241 ms per encode against MiniLM's ~35 ms, which is the 24x1024 shape

Its own fixture set is committed (`d1024-l24-v250002`), so this is now covered rather than assumed — and it was worth doing: it caught the fixture generator hardcoding `pooling: "mean"`, which was invisible while every checkpoint it had seen was mean-pooled.

**Remaining caveat:** the mirror's licence is undeclared where BAAI's original is MIT. Check before depending on it.

---

### aikit scaffold removed

The fixtures it produced are committed and are what the gate reads, so nothing in the build or test path referenced it — the suite passes with the directory deleted. It was also carrying a 4 MB compiled binary into git, committed by accident.

Regenerating for a new checkpoint means restoring two files from history;  names the commit. It was always a separate module, so restoring it adds no dependency to this one.
