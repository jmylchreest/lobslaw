---
sidebar_position: 1
---

# Configuration

lobslaw is configured via a single `config.toml` per node. Some files complement it:

- `.env` — secrets the config refers to via `*_ref = "env:..."` lookups.
- `SOUL.md` — operator-authored markdown describing the agent's persona.
- `certs/` — mTLS material.

## Layout

For single-node deployments (the common case), one `config.toml` is the whole picture.

When you grow to multi-node, `config.toml` becomes one of two things on each node:

1. **Identical across nodes** — provider lists, policy rules, channels. Most operators keep these in version control and ship them via the same path everywhere.
2. **Per-node** — node ID, raft listen address, cert paths. These are the things `lobslaw init` generates per host.

The recommended pattern at that point is a shared base config + a small per-node overlay, merged at boot. The `[config]` block governs how includes work.

## Top-level sections

| Section | Purpose |
|---|---|
| `[cluster]` | Node ID, raft listen address, peer list, mTLS paths |
| `[memory]` | Bolt store path, encryption ref, snapshot tuning |
| `[storage]` | Workspace + skill-tools mount declarations |
| `[security]` | Egress, OAuth providers, clawhub, sandbox |
| `[policy]` + `[[policy.rules]]` | Tool ACLs |
| `[compute]` + `[[compute.providers]]` | LLM provider router |
| `[gateway]` + `[[gateway.channels]]` | User-facing channels (Telegram, REST, webhooks) |
| `[mcp.servers.<name>]` | External MCP servers wired into the tool registry |
| `[scheduler]` | Cron task storage + executor tuning |
| `[skills]` | Skill discovery + invoker tuning |
| `[soul]` | SOUL.md path, fragment count, dream cadence |
| `[hooks]` | Pre/PostToolUse + sessionStart hook scripts |
| `[audit.local]` / `[audit.raft]` | JSONL audit log destination |
| `[discovery]` | Cluster peer discovery (broadcast, DNS, static) |
| `[observability]` | Metrics + tracing |

The full schema lives in `pkg/config/config.go` — every field has a `koanf` tag matching its TOML key, every field has a doc comment.

## Where to dig in

- [Reference](/configuration/reference) — every section, every field, type and default
- [Policy rules](/configuration/policy-rules) — how to write `[[policy.rules]]` entries
- [Providers](/configuration/providers) — LLM provider router + capability discovery
- [Channels](/configuration/channels) — Telegram, REST, webhooks
- [Storage mounts](/configuration/storage-mounts) — workspace, skill-tools, custom mounts
