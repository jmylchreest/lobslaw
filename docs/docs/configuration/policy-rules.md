---
sidebar_position: 3
---

# Policy Rules

How to write `[[policy.rules]]` entries.

For the conceptual model, see [Security → Policy engine](/security/policy-engine). This page is the operator-facing how-to.

## Anatomy

```toml
[[policy.rules]]
id          = "owner-soul-tools"
description = "Owner can mutate soul fragments"
priority    = 20
effect      = "allow"
subject     = "scope:owner"
action      = "tool:exec"
resource    = "soul_*"
```

| Field | Required | Type | Notes |
|---|---|---|---|
| `id` | yes | string | Unique within the rule set; surfaces in audit logs |
| `description` | no | string | Free text |
| `priority` | yes | int | Higher = wins; see priority table |
| `effect` | yes | enum | `allow`, `deny`, `require_confirmation` |
| `subject` | yes | string | `kind:value` form — `scope:owner`, `user:alice`, `channel:telegram`, `subject:google:1234567890` |
| `action` | yes | string | `tool:exec`, `shell:run`, `memory:write`, `credentials:read`, `credentials:grant`, `oauth:start`, `clawhub:install` |
| `resource` | yes | string | Glob — `*` matches everything; `soul_*` prefix; `*.send` suffix |

## Priority conventions

| Range | Use |
|---|---|
| 1 | Default-allow seeds (built-in tools) — auto-seeded, don't write yourself |
| 10 | Default-deny seeds (sensitive built-ins) — auto-seeded |
| 20–99 | Operator-declared allow rules |
| 100+ | Overrides + `require_confirmation` for risky tools |
| 1000+ | Hard denies (revoked subjects, emergency stop) |

Higher number wins on conflict. Within the same priority, the engine sorts by id deterministically — but you should always pick distinct priorities so the resolution is obvious.

## Subject matching

`kind:value` — bare strings (e.g. `owner` instead of `scope:owner`) match nothing. Available kinds:

| Kind | Source | Example |
|---|---|---|
| `scope` | `Claims.Scope` from gateway auth | `scope:owner`, `scope:public` |
| `user` | `Claims.UserID` | `user:alice` |
| `channel` | originating channel type | `channel:telegram`, `channel:rest` |
| `subject` | `Claims.Subject` (OAuth) | `subject:google:1234567890` |
| `*` | matches anything | `*` |

The engine also supports comma-separated alternation in `subject`:

```toml
subject = "scope:owner,user:alice"   # owner OR user alice
```

## Resource glob

Single `*` wildcard, prefix or suffix:

| Pattern | Matches |
|---|---|
| `current_time` | exactly that name |
| `soul_*` | `soul_tune`, `soul_list`, `soul_history` |
| `*.send` | `gws-workspace.gmail.send`, `slack.message.send` |
| `*` | every resource (DANGEROUS — only for `priority=1000+` denies) |

Multiple wildcards in the middle (`gws.*.send`) are not supported. Either widen the pattern or write multiple rules.

## Common patterns

### Open up a sensitive built-in to the operator

```toml
[[policy.rules]]
id       = "owner-can-grant-credentials"
priority = 20
effect   = "allow"
subject  = "scope:owner"
action   = "tool:exec"
resource = "credentials_grant"
```

Needed for: `oauth_start`, `oauth_status`, `credentials_*`, `clawhub_install`, `soul_*`, anything you want to use that defaults deny.

### Allow a skill family for owner

```toml
[[policy.rules]]
id       = "owner-can-call-gws-workspace"
priority = 20
effect   = "allow"
subject  = "scope:owner"
action   = "tool:exec"
resource = "gws-workspace.*"
```

### Require confirmation on writes

```toml
[[policy.rules]]
id       = "confirm-before-sending-mail"
priority = 50
effect   = "require_confirmation"
subject  = "scope:owner"
action   = "tool:exec"
resource = "gws-workspace.gmail.send"
```

The agent pauses, the channel asks `[Yes / No]`, the human decides. This is the **single most effective defence against prompt injection** for write tools.

### Stop being asked about a family of shell commands

**Reach for `[compute] approval_mode` first.** Every command is classified by what it does before
the gate asks, and the shipped default already runs read-only commands without asking. If you are
being asked about `uname -a` and `git status`, the mode is doing something other than the default;
if you are being asked about ordinary local writes and would rather not be, `approval_mode =
"trusted"` is the one-line answer.

Approval is a **subset check** against a set of labels, so it is not limited to the three presets:

```toml
approval_mode = ["reads", "writes", "deletes"]   # a throwaway build box
```

See [the policy engine](/security/policy-engine) for the label vocabulary. `unreadable` cannot be
approved by any spelling, and a command carrying several labels needs all of them approved — so
the rules below remain the way to say something narrower than a whole label.

Every `shell_command` call is then asked about individually, and *Always allow* grants that one
command — so approving `git status --short` leaves `git push --force` still asked about. That is
deliberate: no rule about argv shape separates `git status` from `ssh host`, so nothing is
generalised automatically.

Generalising is an operator decision, written down where it is visible and revocable:

```toml
[[policy.rules]]
id       = "james-git-is-fine"
priority = 20
effect   = "allow"
subject  = "user:tg-@james"
action   = "shell:run"
resource = "git *"
```

Edit the config **once**, rather than approving every git command forever. The
[hardline floor](/security/hardline-floor) still applies underneath — this cannot open up
`rm -rf /`.

Commands with no stable form — pipelines, `&&`, redirects, globs, `$` — are asked about every
time and offer no *per-command* scope button, because no grant could honestly name them. They
evaluate under the reserved resource `!unclassified`; an `allow` on it is the explicit "stop
asking me about compound commands".

Those are exactly the commands an environment-probing agent produces, which is why a confirmation
also offers **Allow reads + writes here** when every label the command carries is one a tap may
approve. A label is nameable even when the command is not, and the grant is recorded per label and
scoped to the conversation like any other session grant — so a mode approving `reads` plus a
tapped grant of `writes` satisfies a command doing both.

It is never offered when any label is `deletes`, `disrupts`, `network`, `privilege` or
`unreadable` — including when the rest of the set would have been fine on its own.

### Allow a public visitor to call read-only tools

```toml
[[policy.rules]]
id       = "public-read-only"
priority = 20
effect   = "allow"
subject  = "scope:public"
action   = "tool:exec"
resource = "current_time"
```

Strangers can ask the time, nothing else. Add to taste.

### Allow MCP tools

MCP tools come from external servers; operator opts in here. Resource patterns support glob:

```toml
[[policy.rules]]
id       = "owner-can-call-minimax"
priority = 20
effect   = "allow"
subject  = "scope:owner"
action   = "tool:exec"
resource = "minimax.*"
```

Tighten by listing specific names:

```toml
resource = "minimax.text_to_image"
```

## Hot reload

`SIGHUP` re-reads `config.toml` and replaces the rule set. New rules apply on the next tool call; in-flight calls keep the rules they evaluated against.

## Auditing what fired

Every evaluation logs to `audit/audit-YYYYMMDD.jsonl`:

```json
{"ts":"2026-04-28T13:45:01Z","action":"tool:exec","resource":"clawhub_install","subject":"scope:owner","decision":"allow","matched_rule":"owner-clawhub-install","priority":20}
```

Search this file when a tool isn't behaving the way you expected — it usually shows immediately whether the rule matched.

## Reference

- `internal/policy/engine.go` — evaluation core
- `internal/policy/rules.go` — TOML schema
- `internal/node/wire_seeds.go` — auto-seeded defaults
- `deploy/docker/config.toml` — annotated example
