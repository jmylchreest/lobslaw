---
sidebar_position: 3
---

# Policy Engine

The gate every tool call passes through.

## What it does

For every `Executor.Call(ctx, tool, args)`, the policy engine asks:

> Given these claims, this action, this resource, and the rules in the store — **allow**, **deny**, or **require_confirmation**?

If the answer is `deny`, the call returns an error before the tool runs. If it's `require_confirmation`, the call is paused until a human-in-the-loop confirms via the originating channel. Allow is silent and proceeds.

## Inputs

```go
type EvaluateInput struct {
    Claims   *types.Claims     // who is calling: scope, user_id, channel
    Action   string            // "tool:exec", "credentials:read", ...
    Resource string            // tool name, credential ID, ...
    Context  map[string]string // optional turn context
}
```

The action+resource shape is the matching key. Conventions:

| Action | Resource shape | Used for |
|---|---|---|
| `tool:exec` | tool name (`current_time`, `notify`, `gws-workspace.gmail.send`) | Every agent tool call |
| `credentials:read` | credential ID | `credentials_grant` invoker side |
| `credentials:grant` | role / skill name | granting a skill access |
| `oauth:start` | provider name | starting a device flow |
| `clawhub:install` | bundle path | installing skill bundles |

`tool:exec` is the dominant action; the others are mutator-specific.

## Rule shape

A rule is a TOML `[[policy.rules]]` block:

```toml
[[policy.rules]]
id          = "owner-soul-tools"
description = "Owner can mutate soul fragments"
priority    = 20
effect      = "allow"             # allow | deny | require_confirmation
subject     = "scope:owner"       # kind:value — see Subject matching below
action      = "tool:exec"
resource    = "soul_*"             # glob — * prefix or suffix
```

**Subject matching** uses `kind:value` form. Common kinds:

- `scope:owner`, `scope:public` — scope claims
- `user:alice` — specific user ID
- `channel:telegram` — channel type
- `subject:google:1234567890` — OAuth subject

Multiple rules per request? The engine sorts by `priority` (descending) and takes the first match's effect. If nothing matches and the resource has a default-allow seed (built-in tools at priority 1), allow. Otherwise deny.

**Priorities, by convention:**

| Range | Use |
|---|---|
| 1 | Default-allow seeds (built-in tools) |
| 10 | Default-deny seeds (sensitive built-ins) |
| 20–99 | Operator-declared allow rules |
| 100+ | Operator-declared overrides + `require_confirmation` for risky tools |
| 1000+ | Hard denies (e.g. revoked subjects) |

A higher number wins. Within the same priority, the engine is deterministic (sort by id) but you should never rely on it — pick distinct priorities.

## Default seeds

On first boot, `internal/node/wire_seeds.go` writes a fixed set of rules:

- **Allow** every `BuiltinScheme` tool (the in-process built-ins) at priority 1.
- **Deny** every sensitive built-in (`oauth_*`, `credentials_*`, `clawhub_install`, `soul_*`) at priority 10. The operator overrides these with priority-20 allows in `config.toml`.

Skills, MCP servers, and clawhub-installed tools are **not** seeded. They're invisible to the agent until the operator adds an allow rule:

```toml
[[policy.rules]]
id       = "owner-can-call-gws-workspace"
priority = 20
effect   = "allow"
subject  = "scope:owner"
action   = "tool:exec"
resource = "gws-workspace.*"
```

## `require_confirmation`

For destructive tools (anything that writes off-host, sends a message, modifies external state), prefer:

```toml
[[policy.rules]]
id       = "confirm-on-write"
priority = 50
effect   = "require_confirmation"
subject  = "scope:owner"
action   = "tool:exec"
resource = "gws-workspace.gmail.send"
```

The engine pauses the call, asks the originating channel for `[Yes / No]`, and proceeds based on the human's reply. This is the **primary defence against prompt injection** for write tools — narrower than blocking, narrower than sandbox.

## Why skills can't impersonate built-ins

Built-in tools live under `BuiltinScheme://` paths. The `internal/compute/registry.RegisterExternal` rejects any registration whose Path begins with that scheme — so a skill manifest claiming `path = "builtin://current_time"` is rejected at install time.

This means the priority-1 default-allow seed for built-ins never applies to non-built-in code. Skills, MCP, and clawhub-installed tools always traverse the operator-declared ruleset.

## Audit

Every policy evaluation that results in a `tool:exec` produces an audit record:

```json
{"ts":"2026-04-28T13:45:01Z","action":"tool:exec","resource":"clawhub_install","subject":"scope:owner","decision":"allow","matched_rule":"owner-clawhub-install","duration_ms":1.4}
```

These land in `audit/audit-YYYYMMDD.jsonl` and (if `[audit.raft]` is set) replicate via Raft to peers. See [Operating → Audit](/operating/cli) for retrieval.

## Hot reload

`SIGHUP` reloads `config.toml`, including the `[[policy.rules]]` blocks. New rules apply on the next call; in-flight calls keep the rules they evaluated against. There is no "restart needed" workflow for policy changes.

## Reference

- `internal/policy/engine.go` — evaluation core
- `internal/policy/rules.go` — TOML schema
- `internal/node/wire_seeds.go` — default seeds
- `internal/audit/` — audit log writer
