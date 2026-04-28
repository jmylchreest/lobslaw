---
sidebar_position: 1
slug: /
---

# What is lobslaw?

**lobslaw** is a self-hosted personal assistant.

It runs as one node — or, if you want resilience, several nodes that share a Raft-replicated store of memory, scheduled tasks, commitments, and credentials. Either way, it exposes a single agent that you talk to over Telegram, REST, or whatever channel you wire up. The agent is yours: it remembers what you told it last week, schedules tasks, holds open promises ("ping me when…"), and calls out to skills you've installed (Google Workspace, GitHub, custom binaries) — every call gated by a policy engine, every subprocess sandboxed, every byte of egress routed through a forward proxy with a per-role ACL.

It is **not** a SaaS and **not** a chat-only LLM wrapper.

## Shape of the system

Single-node — the common case:

```
┌────────────┐
│   node-1   │   ← all subsystems in one process
│ ┌────────┐ │
│ │ memory │ │   ← bbolt
│ ├────────┤ │
│ │ agent  │ │   ← LLM provider router
│ ├────────┤ │
│ │  ...   │ │
│ └────────┘ │
└──┬─────────┘
   │ smokescreen (per-role egress ACL)
   ▼
   internet
```

Multi-node — when you want fault tolerance:

```
┌────────────┐  ┌────────────┐  ┌────────────┐
│   node-1   │  │   node-2   │  │   node-3   │   ← mTLS mesh, Raft consensus
│ ┌────────┐ │  │ ┌────────┐ │  │ ┌────────┐ │
│ │ memory │◄┼──┼─┤ memory │ ├──┼►│ memory │ │   ← bbolt + atomic.Pointer
│ ├────────┤ │  │ ├────────┤ │  │ ├────────┤ │
│ │ agent  │ │  │ │ agent  │ │  │ │ agent  │ │
│ └────────┘ │  │ └────────┘ │  │ └────────┘ │
└──┬─────────┘  └────────────┘  └────────────┘
   │ smokescreen (per-role egress ACL)
   ▼
   internet
```

The same binary, the same config schema, the same data path. The difference is whether `[cluster] peers` has anything in it.

## Headline properties

- **Single-node by default, cluster-capable.** Most operators run one node. The data path uses Raft regardless — a single-node "cluster" still has a Raft FSM and snapshot/restore — so adding a second node later doesn't change semantics.
- **Channel-agnostic.** Telegram is the most-tested gateway. REST + webhooks ship; Slack/Discord are wire-compatible but unimplemented.
- **Policy-gated tools.** Every tool call (built-in, skill, MCP) goes through the policy engine. Builtins ship default-allow at low priority; skills, MCP, and sensitive built-ins (`oauth_*`, `credentials_*`, `clawhub_install`) are default-deny — the operator opens them in `config.toml`.
- **Sandboxed subprocesses.** Skills run with Linux user namespaces, mount namespaces, Landlock, seccomp BPF, and (optional) nftables egress redirect.
- **Persistent + proactive.** Episodic memory + semantic search + cron scheduler + commitments. The agent can promise to do things later and follow through without prompting.

## Where to start

- **Just want to run it?** → [Getting Started → Docker Compose](/getting-started/docker-compose)
- **Curious about security posture?** → [Security → Threat Model](/security/threat-model)
- **Want to write a skill?** → [Features → Skills](/features/skills)
- **Hacking on the codebase?** → [Architecture → Cluster](/architecture/cluster)
