---
sidebar_position: 1
---

# Skills

A **skill** is a sandboxed subprocess that exposes a set of tools to the agent. Skills are the standard extension point — anything you'd want a third-party app to do (read your gmail, post to slack, query a database) is a skill.

## Anatomy

A skill is:

```
my-skill/
  manifest.yaml         ← what tools it exposes, what it needs
  bin/
    my-skill            ← the binary the invoker spawns
  skills.md             ← optional human-facing docs
```

Manifest:

```yaml
schema_version: 1
name: gws-workspace
version: 1.0.0
description: Google Workspace skill — gmail, calendar, drive

# Tools exposed to the agent
tools:
  - name: gmail.search
    description: Search gmail for messages matching a query.
    parameters_schema:
      type: object
      properties:
        query:
          type: string
        max_results:
          type: integer
      required: [query]
    risk_tier: reversible
    argv: ["gmail", "search", "{{query}}", "--max", "{{max_results}}"]

  - name: gmail.send
    description: Send a gmail message.
    parameters_schema: {...}
    risk_tier: communicating
    argv: ["gmail", "send", ...]

# Capabilities the skill needs
mounts:
  - mount: workspace
    subpath: gws-workspace/cache
    mode: rwx
  - mount: skill-tools
    mode: ro

network:
  - oauth2.googleapis.com
  - www.googleapis.com
  - gmail.googleapis.com

# OAuth credentials the skill needs
credentials:
  - role: google
    required: true

# Sandbox toggles
sandbox:
  network_isolation: true       # spawn in own netns; egress via UDS
  no_new_privs: true
  landlock: true
  seccomp: deny-default
```

## Lifecycle

```
┌──────────────────────────────────────────────────────────────┐
│  1. install                                                  │
│     clawhub_install / lobslaw plugin install / manual drop   │
│     ─► writes to skill-tools mount                           │
│     ─► fsnotify picks up manifest.yaml                       │
│     ─► registry.RegisterExternal validates + registers       │
└──────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌──────────────────────────────────────────────────────────────┐
│  2. policy gate                                              │
│     Operator adds [[policy.rules]] resource = "gws-*"        │
│     ─► tool now visible to agent                             │
└──────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌──────────────────────────────────────────────────────────────┐
│  3. credential grant                                         │
│     credentials_grant skill=gws-workspace                    │
│                       cred=google:1234                       │
│                       scopes=[gmail.readonly, ...]           │
└──────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌──────────────────────────────────────────────────────────────┐
│  4. agent calls a tool                                       │
│     policy.Evaluate ─► allow                                 │
│     skills.Invoker.Spawn:                                    │
│       - resolve credentials, refresh if expiring             │
│       - build sandbox.Policy from manifest                   │
│       - exec.Cmd /proc/self/exe sandbox-exec -- bin/...      │
│       - env: HTTPS_PROXY, LOBSLAW_CRED_GOOGLE_TOKEN, ...     │
│     subprocess runs, returns JSON, agent narrates            │
└──────────────────────────────────────────────────────────────┘
```

## Risk tiers

Each tool declares a risk tier. The agent uses these for narration (whether to confirm) and the policy engine uses them to set sensible defaults:

| Tier | Examples | Default policy stance |
|---|---|---|
| `read_only` | `current_time`, `read_file`, `gmail.search` | allow on `scope:owner` |
| `reversible` | `commitment_create`, `schedule_create` | allow on `scope:owner` |
| `mutating` | `soul_tune`, `credentials_grant` | allow on `scope:owner` |
| `communicating` | `notify`, `gmail.send`, `slack.post` | `require_confirmation` recommended |
| `destructive` | `gmail.delete`, `drive.trash_empty` | `require_confirmation` mandatory |

The agent prefers narrating before destructive calls; the policy engine enforces.

## Network and credential isolation

A skill's manifest declares:

- **Mounts** — which storage labels (and optional subpaths) it can see.
- **Network** — which hostnames it can reach.
- **Credentials** — which OAuth roles it expects (`role: google` looks up a credential ACL keyed by skill+role).

The invoker:

1. Resolves the credential (refreshes if access token expires within 5 min).
2. Env-injects `LOBSLAW_CRED_<role>_TOKEN` (the access token only — never the refresh token).
3. Builds `HTTPS_PROXY=http://skill%2F<name>:_@<proxy>:<port>` for egress role tagging.
4. Builds `sandbox.Policy` from the mount + sandbox manifest blocks.
5. Spawns through `sandbox.Apply(cmd)` which rewrites to the reexec helper.

The skill never sees the refresh token, never sees other skills' credentials, never sees mounts it didn't declare.

## Local development

For developing your own skill without the install pipeline:

```bash
mkdir -p /var/lib/lobslaw/skills/my-skill
cp manifest.yaml /var/lib/lobslaw/skills/my-skill/
cp bin/my-skill   /var/lib/lobslaw/skills/my-skill/bin/
chmod +x /var/lib/lobslaw/skills/my-skill/bin/my-skill
```

The fsnotify watcher picks it up. Add a `[[policy.rules]]` allow for `my-skill.*` and the agent can call it.

## Reference

- `internal/skills/skill.go` — manifest parsing + validation
- `internal/skills/invoker.go` — spawn pipeline (cred fetch, env, sandbox)
- `internal/storage/watcher.go` — fsnotify-based discovery
- `internal/compute/registry.go` — registry RegisterExternal (rejects builtin scheme paths)
- [ClawHub](/features/clawhub) — the recommended distribution channel
