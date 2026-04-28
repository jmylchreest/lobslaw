---
sidebar_position: 2
---

# Threat Model

What lobslaw defends against, what it doesn't, and the explicit trust boundaries.

## Targets

The system is built to protect:

1. **The operator's credentials** — OAuth refresh tokens, API keys, anything in the credentials bucket. These are the most valuable thing the cluster holds.
2. **The operator's data** — episodic memory, soul fragments, ongoing commitments. Anything written via the Raft log.
3. **The host** — the operator's machine running the cluster, including paths outside the workspace.
4. **The cluster's integrity** — quorum, leadership, snapshot consistency under hostile peers.

## Adversaries

In rough order of likelihood:

### 1. Compromised or malicious skill subprocess

A skill the operator installed has a bug, was supply-chain-compromised, or was deliberately malicious.

**Defended by:** sandbox (namespaces + Landlock + seccomp), egress ACL, declared mounts only, no setuid escalation (NoNewPrivs).

### 2. Prompt injection

The agent reads attacker-controlled content (a fetched URL, a forwarded message, a file in the workspace) and is steered into issuing a tool call the operator didn't intend.

**Defended by:** policy engine (`require_confirmation` for risky tools), PreToolUse hooks (semantic checks), registry constraints (a `read_file` restricted to `/workspace` has limited blast radius even if invoked maliciously), human-in-the-loop confirmations for destructive tools.

The sandbox does **not** defend against this — it bounds blast radius once a tool is running, but cannot tell a legitimate `notify(text="..")` from an injected one. For tools that write, execute, or send data off the host, `require_confirmation` is the right defence, not tighter sandboxing.

### 3. A non-operator user on a shared channel

Someone messages your bot who isn't you. Their messages enter the agent loop with the `scope:public` (or whatever `unknown_user_scope` you've configured) claim.

**Defended by:** policy engine — every sensitive tool's allow rules are scoped (`subject = "scope:owner"`). Strangers default-deny on credentials, OAuth, soul mutations. Rate-limit + per-channel quotas live at the gateway.

### 4. A network adversary on the path between nodes (multi-node only)

Someone on the LAN, or a malicious peer reachable on the cluster network.

**Defended by:** mTLS for all inter-node traffic (Raft + gRPC + gateway), per-cluster CA signing every node cert, atomic cert rotation via SIGHUP. A peer with a forged or stale cert can't join consensus.

For single-node deployments this is moot — there is no inter-node traffic.

### 5. A compromised peer node (multi-node only)

One of the cluster's nodes is fully compromised — the attacker has the node's private key, can read the bolt store, can submit Raft entries.

**Defended by:** intentionally limited. Once a peer is compromised, the credentials it held are exposed (encrypted with the cluster MemoryKey, which every node has). Quorum still requires majority — a single compromised node can't unilaterally rewrite history. **Credential rotation post-incident is the operator's responsibility.** The cluster MemoryKey rotation surface is deferred (see DEFERRED.md).

For single-node, full host compromise = full credential exposure (no peers to slow the attacker down). The defence at that point is the OS, not lobslaw.

### 6. The host operating system

Kernel exploits, hardware side-channels, malicious firmware.

**Out of scope.** lobslaw is a process running under the operator's OS; it inherits whatever the OS provides.

## Explicit trust boundaries

| Boundary | Side trusted | Side untrusted | Mechanism |
|---|---|---|---|
| Agent ↔ tool subprocess | agent | subprocess | sandbox (namespaces + Landlock + seccomp + egress ACL) |
| Agent ↔ MCP server | agent | MCP server | subprocess + egress ACL; **no sandbox** today (MCP servers live longer than a single call; opt-in netns is on the roadmap) |
| Operator ↔ user-supplied content | operator | content | registry constraints + policy `require_confirmation` + PreToolUse hooks |
| Cluster ↔ peer | cluster member | peer (until mTLS handshake) | mTLS with cluster CA verification |
| User ↔ gateway | gateway | user (until claim resolution) | OAuth/JWT or channel-specific (Telegram chat ID + user prefs binding) |
| Storage at rest | reader holding MemoryKey | bytes on disk | bbolt encryption per-bucket via `crypto.Seal` AEAD |

## What the sandbox doesn't cover

The sandbox is the **last line** — it bounds blast radius after policy, hooks, and registry constraints have all let an invocation through. It cannot:

- Distinguish a legitimate tool call from a prompt-injected one. That's a semantic problem the policy engine and hooks address upstream.
- Stop a tool that has been *granted* a scope from using it. If you grant a skill `gmail.readwrite`, it can read and write your gmail.
- Prevent kernel exploits. CLONE_NEWUSER + Landlock + seccomp narrow the kernel attack surface but don't eliminate it.

The complementary defence layers are documented in [policy engine](/security/policy-engine), [sandbox](/security/sandbox), and [egress and ACL](/security/egress-and-acl).

## Out of scope (intentionally)

- **Multi-tenant isolation.** lobslaw is single-operator-per-cluster by design. Multi-user is on the roadmap (the `[[user]]` config block is multi-user-ready) but cross-user privacy is not fully audited yet.
- **Hardware HSM integration.** The cluster MemoryKey lives in `.env` or similar today. Vault / SGX / TPM integration is deferred.
- **Anti-tampering on the audit log.** Audit JSONL is append-only and raft-replicated, but the operator with raw bolt access can rewrite it. A merkle-proof scheme is on the roadmap.
- **Process budget enforcement.** `WaitDelay = 500ms` bounds wall-clock; cgroup v2 CPU/memory limits are deferred (current reliance is on namespace + sandbox alone).
