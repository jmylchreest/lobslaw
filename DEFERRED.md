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

### Attention still uses the dot kernel

Only the six weight matmuls per layer go through the packed GEMM. Attention's Q·K and scores·V operands are computed per call, so packing them costs more than it saves — and they are ~1.4% of the arithmetic at 64 tokens. That share **grows with sequence length** (attention is quadratic, projections linear), so it stops being negligible on long inputs.

**Trigger to revisit:** if `EmbedLong` on real memories shows attention dominating, or when bge-m3's 8k context is used in anger.

---

### Throughput, measured

Linear, with a flat heap. multilingual-e5-base, 32-token records, AVX2:

| records | per record | records/s | heap |
|---|---|---|---|
| 100 | 94.4 ms | 10.6 | 324.6 MB |
| 400 | 90.3 ms | 11.1 | 324.6 MB |
| 1600 | 91.3 ms | 11.0 | 324.7 MB |

So a 10,000-record backfill is about **15 minutes** on the vector build and roughly **40 minutes** on the portable one. `EmbedBatch` costs the same per record as calling `Embed` in a loop, which is what it is.

The heap not moving across 1600 encodes is the useful part: the ~7,900 allocations per encode are all short-lived, so the collector keeps up and nothing accumulates. That closes off the two ways a backfill could have degraded non-linearly.

`TestBackfillScaling` re-measures it; set `LOBSLAW_EMBEDDER_SCALING=1`.

---

### ~7,900 allocations per encode

Every `matmulBT`/`matmulPacked` spawns goroutines, and `hiddenStates` allocates all scratch per call. Measured as NOT a scaling problem (see above) — the heap is flat across 1600 encodes — so this is a constant-factor cost, not a leak. The obvious fix — hoisting scratch onto the `Model` — is exactly what `TestConcurrentEmbedIsSafeAndDeterministic` forbids: it makes concurrent `Embed` a data race. Any fix must be per-call-arena or `sync.Pool`, not a `Model` field.

Raising the parallel threshold to reduce goroutine churn was measured and made things **14% slower**, so this is not the easy win it looks like.

---

### Portable kernel is ~2.8x slower than the vector one

520 ms vs 184 ms at 64 tokens; arm64 and any non-experiment amd64 build gets the portable path. Remaining portable-friendly work: k-blocking for L2 residency, and a better inner loop that does not spill.

**Trigger to revisit:** first arm64 deployment, or any node without `GOEXPERIMENT=simd`.

---

### Still ~5x slower than the reference implementation

184 ms at 64 tokens against ~36 ms for the same model. The gap is attention, allocations, and no cache blocking — all listed above.

---

### e5 asymmetric prefixes are not applied

e5 models are trained with `query: ` on the question and `passage: ` on the stored text. lobslaw applies neither, because `EmbeddingProvider.Embed` takes one string and has no idea which it is being handed.

Measured on 20 paraphrase queries against 20 memories:

| | recall@1 | recall@3 |
|---|---|---|
| with prefixes | 15/20 | 18/20 |
| without (today) | 14/20 | 17/20 |

One hit at each, on `e5-small`. Small but free — the callers all know which side they are on: the episodic ingester embeds passages, the context engine and `memory_search` embed queries. It needs the interface to say so, which is a wider change than it looks because the remote embedder shares it.

**Trigger to revisit:** any work touching `EmbeddingProvider`, or a complaint about recall quality.

---

### Smaller than 471 MB would need quantisation or vocabulary pruning

82% of the download is the token embedding table at F32. Two ways down, neither implemented:

- **int8 the embedding table.** It is a pure lookup — no arithmetic depends on its precision beyond the first add — so per-row int8 with a scale would cut 384 MB to ~96 MB, taking the total to roughly 180 MB. The forward pass is unaffected.
- **Prune the vocabulary.** XLM-R carries 250k tokens for 100+ languages. A node whose users write English and French needs a fraction of that; dropping unused rows and remapping ids is a known technique and would cut deeper than quantisation.

**Trigger to revisit:** a deployment where 471 MB is genuinely the blocker — otherwise this is effort spent on a one-off download.

---

### backfill-embeddings deletions do not survive a restart

The tool writes `state.db` directly, bypassing raft. The node rebuilds state from its log on boot, so a PUT still in the log re-applies and resurrects any key the tool deleted. Writes survive — new records have fresh ids and nothing contradicts them — but removals come back.

Measured: two `--force` runs back to back converge to zero orphans; a node restart between them brings the same five keys back, every time.

Narrow in practice. Re-embedding works and every record ends up with a current vector; what can reappear is a vector stamped with the previous model, and once its source records are gone nothing can return it in a search. It is litter rather than a wrong answer.

The fix is for the tool to propose its deletions through raft rather than writing underneath it, which means it stops being an offline tool.

**Trigger to revisit:** a model migration on a node whose corpus matters, or anyone confused by stale vectors surviving a `--force`.

---

### Only MiniLM is mirrored

`all-MiniLM-L6-v2` (91 MB, Apache-2.0) is mirrored in this project's releases, so the default English configuration fetches nothing from a third party.

The multilingual models are not: `multilingual-e5-small` is 471 MB, and `multilingual-e5-large` (2.2 GB) and `bge-m3` (2.3 GB) exceed a GitHub release asset's 2 GB per-file limit outright — those would need splitting across assets with a reassembly step, or a different host. All three are MIT, so redistribution is permitted; it is the plumbing that is missing.

**Trigger to revisit:** a multilingual deployment that cannot reach HuggingFace, or CI failing on the e5-small download rather than on the code.

---

### bge-m3 is untested

**`BAAI/bge-m3` cannot load at all** — it ships only `pytorch_model.bin`, no safetensors, and a pickle is arbitrary code execution so it is refused. `Shitao/bge-m3` (the author's own repository) has safetensors, is `xlm-roberta`/gelu/F32/24x1024/8194, and should load unchanged — but nothing has run it, and its licence is undeclared where BAAI's original is MIT. At 24 layers x 1024 hidden it is ~3.5x the arithmetic of e5-base, which makes every performance item above proportionally worse. It also needs its own golden fixture set — the committed one is e5-base only.

**Trigger to revisit:** when multilingual + 8k context is actually wanted; wants mmap and the kernel work first.

---

### No integrity verification on downloaded models

`Ensure` records a SHA-256 of what it fetched but has nothing to compare against, so a compromised or truncated mirror is detected only by the load failing. safetensors cannot execute code, which is why this is a deferral rather than a hole, but a `checksum = "sha256:..."` config key would close it.

**Trigger to revisit:** before recommending any download URL that is not a first-party mirror.

---

### aikit scaffold not yet removable

`tools/genfixtures` is a separate module so `aikit` stays out of `go.mod` and out of the binary. Our tokenizer now reproduces the ids exactly, but the scaffold stays: it is the only INDEPENDENT source of fixtures, and regenerating them with our own implementation would make the gate self-referential — it would prove only that we still agree with ourselves.

**Trigger to revisit:** never, unless a second independent reference appears. It costs nothing: separate module, absent from `go.mod`, absent from the binary.
