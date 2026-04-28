---
sidebar_position: 1
---

# Security Framework

lobslaw's security posture is **defence-in-depth with explicit trust boundaries**. No single layer is the whole answer; every layer composes with every other.

## Layers, top to bottom

```
┌─────────────────────────────────────────────────┐
│  1.  Channel auth (mTLS for cluster, OAuth/JWT  │  ← who is this?
│      for users; gateway-level)                  │
├─────────────────────────────────────────────────┤
│  2.  Policy engine                              │  ← are they allowed?
│      tool:exec, action+resource matching,       │     (per call)
│      priority-ordered allow/deny rules          │
├─────────────────────────────────────────────────┤
│  3.  PreToolUse hooks                           │  ← does the call
│      arbitrary code, can block invocation       │     make sense?
├─────────────────────────────────────────────────┤
│  4.  Sandbox                                    │  ← if the tool runs,
│      Linux namespaces (user/pid/mnt/net)        │     what can it
│      Landlock LSM                               │     touch?
│      seccomp BPF deny-list                      │
│      NoNewPrivs, optional nftables egress       │
├─────────────────────────────────────────────────┤
│  5.  Egress ACL                                 │  ← if it talks
│      smokescreen forward proxy with             │     out, where?
│      per-role host allowlist                    │
├─────────────────────────────────────────────────┤
│  6.  Storage                                    │  ← if it persists,
│      mTLS between nodes, Raft log,              │     how is it
│      bbolt + crypto.Seal AEAD for credentials   │     protected?
└─────────────────────────────────────────────────┘
```

Read the layer docs in order, top to bottom, for the full picture:

1. [Threat model](/security/threat-model) — what we defend against and what's out of scope
2. [Policy engine](/security/policy-engine) — the gate every tool call passes through
3. [Sandbox](/security/sandbox) — namespaces, Landlock, seccomp, the reexec helper
4. [Egress and ACL](/security/egress-and-acl) — smokescreen, role tagging, ratchet defaults
5. [mTLS](/security/mtls) — cluster auth, cert rotation, atomic.Pointer hot-reload
6. [OAuth and credentials](/security/oauth-and-credentials) — RFC 8628 device flow, encrypted-at-rest, refresh on spawn

## Default posture

| Subject | Default |
|---|---|
| Built-in tools (`current_time`, `read_file`, `notify`, …) | **Allow**, seeded at priority 1 |
| Sensitive built-ins (`oauth_*`, `credentials_*`, `clawhub_install`, `soul_*`) | **Deny** unless operator adds an explicit `[[policy.rules]]` allow |
| Skills | **Deny** unless operator adds an explicit allow per scope |
| MCP-server tools | **Deny** unless operator adds an explicit allow per scope |
| Egress | Direct deny for RFC1918; per-role allowlist for everything else |
| Subprocess filesystem | Empty mount namespace, Landlock allowlist of declared paths only |

The cluster-default rules are seeded once on first boot via `wire_seeds.go`. Everything else is operator-declared in `config.toml`. There is no implicit "production" or "dev" mode — the same rules ship everywhere; the difference is what the operator opens up.

## Where security work happens

| Concern | Where to look |
|---|---|
| "Can this tool be called by this user?" | `internal/policy/engine.go` |
| "Can this skill spawn?" | `internal/skills/invoker.go` + `internal/sandbox/` |
| "Where can this tool reach on the network?" | `internal/egress/` (smokescreen builder) |
| "How are credentials at rest?" | `internal/memory/credentials.go` + `pkg/crypto/seal.go` |
| "Did this happen?" | `internal/audit/` (JSONL, raft-replicated) |
