# Deferred Work

Items consciously deferred past MVP. Each has a short note on why it's deferred and what would trigger revisiting it.

## Modality routing

### Capability auto-discovery via models.dev

`[[compute.providers]]` carries declared `capabilities = [...]` tags today. Operators keep those in sync with what the model actually supports. Auto-discovery would close that drift gap.

**Best source:** [models.dev](https://models.dev) — community-maintained unified catalog (~200 models across providers) at `https://models.dev/api.json`. Returns structured `modalities`, `cost`, `limit`, `attachment` fields per model. No auth required, single shape for every vendor. Better than per-provider `/models` endpoints because OpenRouter has rich metadata, OpenAI/Anthropic just return model IDs, and MiniMax has nothing.

**Why deferred:** Declared caps work for everyone today. The gain is UX (operator doesn't forget a tag) not capability — nothing's blocked. Adds an external dependency (network at boot) and a freshness story (models.dev cache lag for new model releases).

**Trigger to revisit:** First time an operator forgets to add a capability tag and a `read_*` builtin silently doesn't register despite the model supporting it.

**How:**
1. Add `auto_capabilities = true` flag on `ProviderConfig`.
2. At boot, for each provider with the flag set: fetch `https://models.dev/api.json` (cache to disk for 24h), look up the configured `model` by name, merge discovered `modalities` into `Capabilities`. Declared caps always win on conflict.
3. Fallback chain when models.dev doesn't list the model: try the provider's native `/models` endpoint (per-vendor dispatcher) → still missing → use declared caps only + log INFO.
4. Config flag also enables pricing auto-pull from the same catalog, replacing the hardcoded pricing table over time.

---

### Self-hosted modality sidecars (Parakeet / faster-whisper / LLaVA / ollama-vision)

The capability-driven router already works with self-hosted endpoints — point a `[[compute.providers]]` entry at `http://parakeet:8000/v1/audio/transcriptions` (or wherever) with `capabilities = ["audio-transcription"]` and `read_audio` picks it up. No code change needed.

**Why noted, not deferred:** Architecturally already supported. What's missing is operator examples — a `deploy/docker/sidecars.yml` that bundles faster-whisper-server + ollama-vision so users have a working "no remote API" deployment to copy.

**Trigger to revisit:** Any time someone asks "how do I run this fully offline?" — the answer is the docker-compose example.

---

### Modality fallback chain at runtime

`SelectByCapability` returns providers in priority order, but the modality builtins currently register against only the first match. If the primary provider returns 5xx or rate-limits, the call fails — there's no automatic retry against the next-priority match.

**Why deferred:** Single-provider works fine when only one is configured (the common case today). Adding fallback requires deciding retry policy (5xx only? include 429? exponential backoff between providers?) — premature without operational data.

**Trigger to revisit:** First time the user reports "the bot couldn't read my image, MiniMax was down" while OpenRouter or Anthropic vision was also configured.

**How:** Pass the full ordered match list into `RegisterVisionBuiltin` / `RegisterAudioBuiltin` etc.; on transient HTTP failure, try the next entry. Cap retries at len(matches). Log per-attempt failures so operators can see degradation patterns.

---

## Storage

### Cross-cluster storage tunneling / routing

A storage-enabled node materialises its own mounts locally. A compute-only node (no `storage` function enabled) cannot currently access mounts materialised on another node — it would need to ask a peer to proxy reads/writes.

**Why deferred:** Complex (proxying FUSE operations over gRPC while preserving POSIX semantics), and the MVP deployment pattern is storage-everywhere (every node enables the storage function). The tunneling feature only matters when operators explicitly split storage away from compute.

**Trigger to revisit:** First deployment where an operator wants a pure compute-only node that still needs filesystem paths under `/cluster/store/...` without local mounts.

**How:** Add a `StorageService.Read` / `StorageService.Write` pair that proxies filesystem operations, plus a client-side FUSE mount on the compute-only node that calls those RPCs. ~2-3 weeks of careful work to get POSIX semantics right; probably a FUSE library is the cleanest path.

---

### Strict-security CA bootstrap (sign-RPC mode)

MVP treats CA material as an infrastructure concern (per `lobslaw-cluster-bootstrap`) — every storage-enabled node's initContainer has the CA private key and self-signs. For tighter security, the CA key could live on one node running a `ClusterBootstrap.Join` gRPC; other nodes present a CSR + HMAC proof from a shared join secret.

**Why deferred:** The infrastructure-concern model covers the single-tenant personal-scale threat model. Strict-security bootstrap adds a SPOF during join + HMAC handshake plumbing for marginal security gains at our scale.

**Trigger to revisit:** First deployment where the CA private key must not leave a specific node (regulated environment, dedicated secrets vault, etc.).

---

## Dependencies

### slog-logfilter maintenance monitoring

`github.com/jmylchreest/slog-logfilter` is a personal project (0 stars, 0 forks at adoption). It's the correct fit for lobslaw's runtime log-level + attribute-based filter needs, and the maintainer is the same person running this project — so day-to-day "is it maintained?" is yes.

**Why tracked:** if the library ever goes dormant or needs changes we can't get upstream, we should vendor or fork it in-tree (e.g. `internal/logging/filter/`). Migration path is straightforward: the library's public API is narrow (Option functions + package-level filter mutators) and the integration in `internal/logging/log.go` is ~70 LOC.

**Trigger to revisit:** upstream goes 6+ months without a commit AND we need a change, OR we hit a bug we can't patch upstream.

---

## Memory

### Session-retention pruning policy — LANDED

Implemented as `internal/memory/SessionPruner` plus an auto-seeded `lobslaw-builtin-session-prune` task. Hard-deletes both vector + episodic records tagged `retention=session` whose timestamp is older than `[memory.session].max_age` (default 24h). Default cadence `@hourly`; opt-out via `[memory.session].enabled = false`. Leader-only (soft-skips on followers so duplicate scheduler claims are safe). Wired alongside the dream task seed in `node.seedSessionPruneTask`.

---

## Cluster Membership

### `bootstrap_expect`-style cold-start coordination

Today every node boots with `bootstrap = true` (default) and decides per-node whether to bootstrap or join based on whether it can find a peer. Sequential bringup works (start node-1, wait for election, then nodes 2/3) but a *simultaneous* cold start of N empty-state nodes split-brains: each node fails to find a leader within `bootstrap_timeout`, falls back to solo bootstrap, and you end up with N independent single-voter clusters.

**Why deferred:** The simultaneous-cold-start case only arises in fully automated provisioning (k8s StatefulSet rollout, Terraform apply, etc.) where an operator can't reasonably stagger startups. Personal-cluster + container-compose deployments are sequential by nature (`compose up -d node-1` then `compose up -d node-2 node-3`), and that workflow is documented in `deploy/docker/cluster.yml`. The current discovery code (broadcast + echo-on-new-peer in `internal/discovery/broadcast.go`) is enough for the workflows we have today.

**Trigger to revisit:** First fleet-style deployment (k8s StatefulSet of N replicas, or Nomad job with `count = N`) where N nodes come up in parallel against an empty-state cluster.

**How:** Consul-style `bootstrap_expect = N` in `[cluster]`. Behaviour:
1. All nodes wait until they've discovered N-1 peers via `[discovery]` (broadcast or seeds), with addresses + node IDs collected.
2. Once everyone agrees on the set of N nodes, the lexicographically smallest NodeID calls `BootstrapCluster` with the *full* N-voter `raft.Configuration`.
3. The other N-1 nodes detect the bootstrap (via raft replication / a follow-up announce field) and skip their own bootstrap path.
4. Adds ~150 LOC: a sync barrier in `internal/node/establishRaftMembership`, an `in_cluster` flag added to `internal/discovery/broadcast.go::packet`, and a config field. Bigger lift is the test surface — needs a multi-node integration test where N processes start within a tight window.

Until then, the documented workaround is "bring up node-1 first, wait for `raft leadership changed is_leader=true` in its logs, then start the rest." Compose's `depends_on: { lobslaw-1: { condition: service_started } }` handles this for the docker stack.

---

### Leadership-loss callback dropped under contention — LANDED

Fixed in `internal/memory/raft.go`. Three callback paths now feed `onLeadership`: (1) the original `LeaderCh()`, (2) every `raft.Observation` event re-publishes the current state via `publishLeadership()` which reads `Raft.State()` fresh, and (3) a 1s reconcile ticker as the safety net for cases where both other signals miss an edge. `SetLeadershipCallback` also seeds with the current state immediately so a transition that happened between `NewRaft` and the wire-up isn't lost. `LeaderGate.Publish` is idempotent (no-op on same value), so over-firing is cheap and double-publishing across paths is harmless. Tested: `TestSetLeadershipCallbackSeedsCurrentState`, `TestPublishLeadershipReconciles`.

---

## MCP

### `.mcp.json` discovery wireup (Claude Code / OpenCode parity) — PARTIAL

`.mcp.json` discovery is wired in `cmd/lobslaw/main.go` for the **config directory** (alongside `config.toml`) — operator-controlled and trusted. Discovered servers merge into `cfg.MCP.Servers` and load identically to `[[mcp.servers]]` entries. What's still deferred is broader discovery from plugin/skill subdirectories with a trust story:

**Still deferred:** auto-loading from `~/.config/lobslaw/workspace/skills/<name>/.mcp.json` and similar plugin paths — these introduce a new trust surface (anyone with write access can declare an MCP server lobslaw will spawn). Needs signing/allowlist work first.

**Trigger to revisit:** First plugin-style integration that ships its own `.mcp.json` (e.g. an installable lobslaw plugin that declares its MCP server alongside its skills).

---

### Agent-driven MCP install (policy-gated)

Lobslaw could expose an `install_mcp` builtin tool that lets the agent install an MCP server at runtime via chat ("install minimax-mcp and use text_to_image"). The current `[[mcp.servers]] install = [...]` field is operator-only — only triggered at boot from config.toml.

**Why deferred + why it stays deferred:** This is a security smell by default. Letting an LLM install arbitrary code is a remote-code-execution surface dressed up as a feature. The OpenCode / Claude Code approach is exactly right: install is operator-curated, agent uses what's available. There's no use case strong enough to justify the risk on a default-on basis.

**Trigger to revisit:** A specific deployment where an operator explicitly wants an agent to manage its own toolchain — e.g. a multi-tenant dev sandbox where each tenant's agent is allowed to add Python packages within a quota. Even then, build it as a guarded builtin that's:
1. Default disabled (operator opts in via config flag).
2. Subject to policy: every install is a `tool:exec install_mcp` event, default-deny, with explicit allow rules pinning specific package@version pairs.
3. Audit-logged (raft sink) so a tampered install is detectable post-hoc.
4. Sandboxed: the install command itself runs under landlock with network access restricted to the curated package registry.

**How (when needed):** A new `compute.builtin_install_mcp` that takes `package` + `version` + `args`/`env` config and adds to a runtime-mutable `[[mcp.servers]]` slice via `MemoryService.Apply`. Replicate via raft so the install propagates; guard at the policy layer with `effect="deny", priority=1000` as the default rule, requiring an explicit higher-priority allow to enable.

---

### Channel attachment abstraction → agent

### Channel attachment abstraction → agent — LANDED (telegram path)

Done for the Telegram path end-to-end:

1. `pkg/types.Attachment` is the shared shape (moved from internal/gateway to pkg/types so compute + gateway share it without an import cycle).
2. Telegram's `handleMessage` populates `IncomingMessage.Attachments` and the downloader writes bytes to `/workspace/incoming/<turn>/<file_id>.<ext>`.
3. `compute.ProcessMessageRequest` gained `Attachments []types.Attachment`.
4. `decorateWithAttachments` appends `[user attached: ...path=...]` to the user-turn text plus per-modality call-this-tool hints (`read_image` / `read_audio` / `read_pdf`).
5. `ProviderConfig.Capabilities` now consumed for routing — `compute.SelectByCapability` discovers vision/audio/pdf-tagged providers and the modality builtins auto-register against them.

**Still deferred:** REST + webhook channel handlers don't yet populate IncomingMessage — they go straight to `compute.ProcessMessageRequest{Message: text}` and skip the abstraction. Provider-native vision passthrough (sending images inline to a multimodal main model rather than via the read_image tool) is also not done — the current shape is "tool-as-vision" via `read_image` which works against any vision-capable endpoint.

**Trigger to revisit:** When the user wants attachments on the REST/webhook channels, OR when they want to swap to a vision-capable main model and bypass the read_image tool hop.

---

### Clawhub.ai chat-driven skill management (`skills.*` builtins, policy-gated)

DESIGN.md describes clawhub-compatible skill format and `lobslaw plugin install clawhub:foo` — but the install side is `errNotImpl` today (`internal/plugins/plugin.go:314`), and there's no chat-side surface. Goal:

- `skills_search` builtin (read-only, browse clawhub registry) — default-allow.
- `skills_list` builtin (read-only, what's installed locally) — default-allow.
- `skills_install` builtin (mutates skills storage mount, runs verifier) — **default-deny via policy**.
- `skills_remove` builtin (mutates skills storage mount) — **default-deny via policy**.

Trust model (matches the MCP install design):

1. Default-deny: `seedDefaultPolicyRules` writes `effect="deny", priority=1000` for `skills_install` + `skills_remove` so the agent CAN'T install or remove without an explicit operator allow.
2. Operator opts in by adding a higher-priority allow rule (e.g. `subject=owner, resource=skills_install, effect=allow, priority=10`) — typed in chat by the operator or via config.
3. Every install runs through the existing skill-trust pipeline: minisign signature verification (when configured), tarball checksum, manifest validation, atomic replace into the skills storage mount.
4. Audit-logged via raft sink so a tampered install is detectable post-hoc.
5. Optional version pin in the install request (`skills_install name=web-search version=1.2.3`) so an agent can't be talked into "install latest of <evil-package>".

**Why deferred:** This is ~400-600 LOC + careful test coverage:
- HTTP client to `clawhub.ai/api/v1/skills/{name,search}` (with timeout, error mapping).
- Tarball fetch + sha256 verification against the manifest's published checksum.
- Optional minisign signature verification (per the existing `SkillsConfig.SigningPolicy`).
- 4 builtins + ToolDefs + handler functions.
- Policy seed for the deny defaults.
- Audit hooks.
- Tests covering: registry unreachable, signature mismatch, version pin honoured, policy deny respected, double-install idempotency, partial-install rollback.

Doing this in a tail-of-session rush is how subtle security bugs (signature bypass, path traversal in tarball extraction, leaking install state across tenants) land. It deserves its own focused session.

**Trigger to revisit:** When you want to run a skill from clawhub without operator-side `git clone + cp` and you've decided which initial skills to support (web-search? RTK? a specific shape).

**How:**
1. New `internal/clawhub/client.go`: HTTP client + types for the clawhub API.
2. New `internal/clawhub/installer.go`: tarball fetch, sha256 verify, minisign verify, atomic replace.
3. New `internal/compute/builtin_skills.go`: 4 builtins with the schema + handlers.
4. Hook into existing `seedDefaultPolicyRules` for the deny seeds.
5. Update `cmd/lobslaw/plugin.go::pluginInstall` to recognise `clawhub:<name>` refs and route to the same installer (operator-side CLI parity).

Until then: clawhub is read-via-CLI only, and the manual workflow is "drop a skill dir into `~/.config/lobslaw/workspace/skills/<name>/` and the registry's fsnotify watcher picks it up".

---

### MCP tool-call observability (now in)

Each MCP tool invocation is logged at INFO with server/tool/qualified-name/arg-keys (keys only — values redacted), and at DEBUG with duration + result-byte-count on success. Failures (transport, server-returned-error) log at WARN with the upstream message. Visible across the cluster via:

```
podman compose logs -f | jq -c 'select(.msg | test("mcp: "; "i"))'
```

This is the diagnostic baseline. Future: Prometheus counter for tool-call count + latency histogram once we add a metrics endpoint.

---

## Infrastructure / Workflow

### Verify SHA pins in `.github/workflows/*.yml`

Phase 1.7 + Phase 2.1 land CI with SHAs pinned from training-data recall:
- `actions/checkout@11bd71901bbe5b1630ceea73d27597364c9af683` (v4.2.2)
- `actions/setup-go@f111f3307d8850f501ac008e886eec1fd1932a9a` (v5.2.0)
- `golangci/golangci-lint-action@a4f60bb28d35aeee14e6880718e0c85ff1882e64` (v7.0.0)
- `bufbuild/buf-action@3f69d0a55ba1a61a3823f84f0cd7a27e0b66e56c` (v1.4.0)

**Why deferred:** SHAs should be verified against the actual published tag at the time of commit, not recalled. Per `gha-action-pinning` we want SHA pinning with version comment — the format is right but the SHA values need confirming on first CI run.

**Trigger to revisit:** First CI run that reports a SHA mismatch, OR as part of routine dependency review (dependabot/renovate config).

**How:** `gh api repos/{owner}/{repo}/git/ref/tags/{tag}` to fetch the authoritative SHA, update the workflow, re-commit. Can be automated via a renovate config with `pinDigests: true`.

---

### Branch protection on `origin`

Configure required checks + PR workflow per `gha-branch-protection` decision.

**Why deferred:** Solo work right now; direct-to-main commits are fine. Adding CI-required checks before collaborators join is friction for no benefit.

**Trigger to revisit:** First external contributor, or first public release.

**How:** `gh api --method PUT repos/jmylchreest/lobslaw/branches/main/protection ...` — or set via repo settings UI.

---

### `storage:<label>[/<path>]` reference scheme — exact semantics

References like `memory.snapshot.target = "storage:r2-backup"` and `audit.raft.anchor_target = "storage:r2-backup"` use a `storage:` scheme against `[[storage.mounts]]` labels.

**Working definition:** `storage:<mount-label>[/<path>]` resolves to `<mount-path>/<path>` inside the compute node's mount namespace, where `<mount-label>` matches a `[[storage.mounts]] label`.

**Open questions to resolve when first consumer lands:**
- Are writes to the target gated by the mount's existing `read_only_paths` config?
- Should `storage:` references support cross-mount fallback (e.g. if primary is unreachable)?
- Is there a canonical way to reference a sub-path within a mount (URL-style, path-style)?

**Trigger to revisit:** Phase 3 (memory snapshot target) or Phase 11 (audit anchor target) — whichever lands first.

---

### mTLS cert rotation hot-swap

Out of scope for MVP per `lobslaw-hot-reload` decision.

**Why deferred:** Go's stock gRPC `credentials.TransportCredentials` doesn't cleanly hot-swap. Needs a custom `TransportCredentials` wrapper that refetches certs on each handshake. ~200 LOC of careful work. Initial deployments will tolerate a restart for cert rotation.

**Trigger to revisit:** First deployment with a short-lived cert regime (ACME-style, hourly rotation) or the first time a restart-to-rotate is user-visible.

---

### OpenCode plugin deep import

Metadata-only adapter in MVP; full TypeScript SDK runtime deferred.

**Why deferred:** OpenCode plugins are TS, loaded in-process in their runtime. Our plugin model is subprocess-based (Claude Code compatible). Running TS plugins would require embedding a V8 or Bun runtime — substantial dep.

**Trigger to revisit:** Only if a must-have plugin exists only in OpenCode format.

---

### clawhub.ai publish

MVP is read/sync only. Publishing (`clawhub push`) is future.

**Why deferred:** Publishing adds auth, versioning, and review-flow complexity. Reading is enough to benefit from the community ecosystem.

**Trigger to revisit:** When we have a skill worth publishing.

---

## Storage Backends

### GCS and Azure Blob backends

MVP supports local dirs + S3-compatible (including MinIO and R2).

**Why deferred:** rclone supports them but each backend adds config surface, tests, and docs. S3-compatible covers ~90% of cases.

**Trigger to revisit:** User demand.

---

## Runtime / Skills

### WASM skill runtime

MVP supports Python, Bash, and Go skill handlers.

**Why deferred:** WASM is the most sandbox-friendly but needs a runtime (wazero is a good candidate — pure Go, CGO-free). Not blocking; our existing sandbox covers Python/Bash/Go adequately.

**Trigger to revisit:** First WASM-only skill in clawhub worth running.

---

### Skill hot-reload for `sidecar: true` skills

MVP hot-reloads normal skills. Sidecar-bearing skills require sidecar restart.

**Why deferred:** Sidecar lifecycle is a separate process; coordinating live restart without dropping in-flight tool calls is non-trivial.

**Trigger to revisit:** First user report that sidecar-reload interrupts their workflow.

---

## Operational

### Advanced Raft reconfiguration

MVP: initial cluster members via seed list + `raft.AddVoter` / `raft.RemoveServer` at runtime.

**Why deferred:** hashicorp/raft handles basic joint-consensus membership cleanly. Advanced cases (splitting a cluster, merging clusters, promoting learners to voters gracefully under load) need more thought.

**Trigger to revisit:** First production topology change that the basic APIs can't express.

---

### LDAP / AD integration

MVP is JWKS-based SSO (Google, Okta, self-hosted OIDC).

**Why deferred:** LDAP is a protocol of its own. JWKS covers all the IdPs we personally use.

**Trigger to revisit:** Corporate deployment requirement.

---

### SLA / health dashboards

MVP exposes `HealthStatus` via gRPC and `/healthz` / `/readyz` on the gateway.

**Why deferred:** Dashboards belong to the operator's observability stack (Grafana, Datadog, whatever). We emit OTel — they build the dashboard.

**Trigger to revisit:** First time we can't diagnose an issue without a dashboard.

---

### Cryptographic confidentiality between scopes

Per `lobslaw-trust-model`: single-tenant by design; `scope` is a routing/audit label, not a confidentiality boundary.

**Why deferred:** Real multi-tenant isolation (per-scope encryption keys, scope-enforced Recall) requires a different architecture. Use a separate cluster instead.

**Trigger to revisit:** Never, probably. If it does become needed, it's a new project.

---

## Non-MVP Performance

### Vector library upgrade (HNSW / FAISS)

MVP uses pure-Go cosine similarity over float32 slices.

**Why deferred:** Adequate for memory < ~1M records. Personal-scale probably never exceeds this.

**Trigger to revisit:** If memory store exceeds ~500k records and recall latency becomes noticeable.

---

### LLM prompt caching for Anthropic

MVP uses OpenAI-compatible API only.

**Why deferred:** Anthropic's native SDK supports prompt caching (3–10× cost/latency win for repeated system prompts). OpenAI-compat mode may or may not expose this cleanly. Need a closer look.

**Trigger to revisit:** First measurable LLM cost pain, or first serious bench of Anthropic as primary provider.

---

### LLM cost-table refresh automation

MVP ships hardcoded pricing defaults per provider.

**Why deferred:** Auto-refresh from provider pricing APIs or a curated GitHub file is nice-to-have. Hardcoded defaults work until the user notices drift.

**Trigger to revisit:** First wrong budget calculation from price changes.

---

## Sandbox

Phase 4.5 lands the sandbox *structure*: Policy type, Validate/Normalise, CanonicalizeAndContain / RequireSingleLink helpers, and Apply wiring that sets Linux namespaces + UID/GID mapping via `syscall.SysProcAttr`. Several deny-in-depth layers are deliberately left as structural-only so the config surface is stable but install is deferred.

### Phase 4.5.5 hardening: NoNewPrivs + Landlock + seccomp BPF — LANDED

Shipped 2026-04-22 across commits `feat(phase-4.5.5a..d)`. The reexec helper (`lobslaw sandbox-exec`) now installs all three kernel enforcement layers between `fork()` and `execve()`. See [`docs/dev/SANDBOX.md`](docs/dev/SANDBOX.md) for the architecture and upstream-tracking notes.

`sandbox.Apply` rewrites any `exec.Cmd` whose Policy carries an enforcement field (NoNewPrivs, AllowedPaths, or Seccomp.Deny) to invoke `/proc/self/exe sandbox-exec --` through the helper child. Namespaces remain orthogonal — they apply via `SysProcAttr.Cloneflags` and don't pay reexec cost.

**Upstream tracking (still active):** [golang/go#68595](https://github.com/golang/go/issues/68595) would collapse Landlock + NoNewPrivs into `SysProcAttr` — when it lands we migrate per the plan in `docs/dev/SANDBOX.md`. Seccomp stays in the helper indefinitely ([#3405](https://github.com/golang/go/issues/3405) is dormant).

---

### pivot_root / chroot into sandbox rootfs — SUPERSEDED

Originally planned as the filesystem-scoping mechanism. **Superseded by Landlock** (see above and `lobslaw-filesystem-sandbox` decision, 2026-04-22).

**Why superseded:** Landlock achieves the same end (tool sees only its allowed paths) with none of the prerequisites — no root, no rootfs construction, no bind-mount management, no cleanup. Kernel-enforced at syscall level, can't be escaped by `/proc/self/root` tricks. Drop-in replacement at a tenth the LOC.

**What stays:** `CLONE_NEWNS` mount namespace (already wired). It's cheap and prevents tool-side `mount` calls from affecting the host mount table — complements Landlock rather than replacing it.

---

### cgroup v2 install (CPU / memory limits)

Policy carries `CPUQuota` (millicpus) and `MemoryLimitMB`. Validate rejects negatives. Apply does not create or attach to a cgroup.

**Why deferred:** Cgroup v2 needs (a) a delegated cgroup (systemd session, user slice), (b) writing `cpu.max` / `memory.max`, and (c) placing the subprocess's pid into `cgroup.procs` before exec. The moving parts are fine on bare Linux but flaky across WSL / older distros / rootless podman. Needs a careful probe + fallback.

**Trigger to revisit:** First user report of a tool runaway. The `WaitDelay` kill path already bounds wall-clock.

**How:** `internal/sandbox/cgroupv2/install.go` — detect `/sys/fs/cgroup/cgroup.controllers`, create a leaf cgroup under our slice, write limits, fork-exec tool with its pid written into `cgroup.procs`. Gate via runtime-detect; no-op with a log warning where cgroup v2 isn't available.

---

### nftables egress rules

Policy carries `NetworkAllowCIDR`. When Apply creates `CLONE_NEWNET` the subprocess has an isolated network stack with only loopback — nothing outbound works. Allowing a specific CIDR requires bringing up a veth, adding a route, and installing nftables rules.

**Why deferred:** Full network-namespace egress needs root (or CAP_NET_ADMIN) on the host to create the veth pair. It's a larger change than the rest of Layer A combined and the MVP policy for most tools is "no network at all" which the bare namespace already gives us.

**Trigger to revisit:** First skill that legitimately needs network egress (e.g. a `web-fetch` tool) and should be constrained to a specific CIDR.

**How:** Either a per-invocation veth + nftables install (needs a helper running as root) or an in-process SOCKS proxy that the subprocess MUST use (simpler, no root, but requires tool cooperation).

---

## Dev Environment Notes

### `get-key` alias

The user's OpenRouter API key is accessed via the zsh alias `get-key OPENROUTER_API_KEY_LOBSLAW`. This is a local-dev convenience, not a lobslaw concern — lobslaw reads `env:OPENROUTER_API_KEY_LOBSLAW` at runtime per `lobslaw-config-env-override`. Local-dev workflow is `OPENROUTER_API_KEY_LOBSLAW=$(get-key OPENROUTER_API_KEY_LOBSLAW) ./lobslaw ...`.
