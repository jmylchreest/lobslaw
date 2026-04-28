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
