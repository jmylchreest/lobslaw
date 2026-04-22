# Deferred Work

Items consciously deferred past MVP. Each has a short note on why it's deferred and what would trigger revisiting it.

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

### Session-retention pruning policy

Records tagged `retention: session` should be pruned aggressively — the whole point of session retention is to keep tool-output and transient context out of long-term memory. Current implementation has no explicit session-prune loop; session records only disappear if they fall below the Dream/REM prune threshold, which isn't their purpose (dream prunes episodic-tier records).

**Why deferred:** session pruning is conversation-lifecycle-aware — the natural trigger is "every N turns" or "on conversation close", both of which live at the Gateway / Channel layer (Phase 6). The memory layer exposes the records tagged `session`; the conversation owner prunes them.

**Trigger to revisit:** Phase 6 (channel handlers + session management) or sooner if session records noticeably accumulate.

**How:** `MemoryService.ForgetSession(session_id)` or a Channel-layer ticker that runs `Forget` scoped to a session tag. Either works.

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

### NoNewPrivs enforcement

Policy carries `NoNewPrivs` and Normalise defaults it to true when any sandboxing is enabled, but `syscall.SysProcAttr` in Go's stdlib does not expose the `NoNewPrivs` field. Go 1.23+ has `SysProcAttr.NoNewPrivs` on some builds; the module targets compatibility that doesn't assume it.

**Why deferred:** Needs either (a) a cgo hop to call `prctl(PR_SET_NO_NEW_PRIVS, 1)` from the child-side `Pdeathsig`-style setup, (b) a wrapper `setpriv --no-new-privs` around every tool invocation, or (c) a minimum Go version bump once stdlib exposes it consistently.

**Trigger to revisit:** Option (c) — bump minimum Go version to one that has `SysProcAttr.NoNewPrivs` across our supported build matrix. Wire it in Apply with a one-liner.

---

### seccomp BPF install

`DefaultSeccompPolicy` carries a deny-list (ptrace, unshare, setns, pivot_root, mount/umount, init_module, kexec_load, bpf, keyctl, etc.) and Normalise applies it when sandboxing is enabled. The install that turns the list into a kernel-enforced filter is not wired.

**Why deferred:** The BPF install needs `elastic/go-seccomp-bpf` (per `lobslaw-seccomp-library` decision) plus careful attention to the arch-specific syscall numbering and the fact that the filter must be installed on the child side *after* exec — cleanest integration is a small wrapper binary that installs the filter then execve's the real tool.

**Trigger to revisit:** Phase 4.5.5 / hardening pass. The Policy config is already stable so no config migration is needed when this lands.

**How:** Write `internal/sandbox/seccomplinux/install.go` using `elastic/go-seccomp-bpf`. Either (a) a tiny `lobslaw-sandbox-helper` binary invoked as argv[0] that installs the filter and execve's, or (b) post-fork / pre-exec install via a cgo hop.

---

### pivot_root / chroot into sandbox rootfs

Apply sets `CLONE_NEWNS` so the subprocess has its own mount namespace, but it doesn't call `pivot_root` to relocate the rootfs. AllowedPaths / ReadOnlyPaths are carried by Policy but not bind-mounted.

**Why deferred:** Building the sandbox rootfs correctly (bind-mount `/proc` into the new PID namespace, mount tmpfs on `/tmp`, set up `/dev` minimally, pivot away from host root) is materially more work than the namespace-clone itself. It's the right thing to do eventually but each layer of sandboxing after the namespace has diminishing returns for a personal-assistant threat model.

**Trigger to revisit:** First deployment profile that includes untrusted tools (cluster-mode running community skills). Before that, the owner-only allowed-paths pattern is enough.

**How:** A `buildSandboxRoot(policy) (rootfsDir, cleanup, error)` that prepares the hierarchy, then Apply invokes a helper child that calls pivot_root after setting up mounts. Similar shape to containerd's shim.

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
