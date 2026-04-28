---
sidebar_position: 1
---

# Built-in Tools

Tools that ship with lobslaw. All have `path` starting with `BuiltinScheme://` and run in-process, no subprocess.

## Time + utilities

| Tool | Risk | Description |
|---|---|---|
| `current_time` | read | Current time in user TZ + UTC |
| `notify` | comm. | Channel-agnostic notification — see [Notifications](/features/notifications) |

## Filesystem

| Tool | Risk | Description |
|---|---|---|
| `read_file` | read | Read text file content |
| `list_files` | read | Glob-list files in declared mounts |
| `search_files` | read | grep-style search across files |
| `edit_file` | mutating | Pattern-based file edit |

All filesystem builtins respect Landlock — they can only see paths the agent's policy allows.

## Web + fetch

| Tool | Risk | Description |
|---|---|---|
| `fetch_url` | read | HTTP GET via smokescreen (`fetch_url` egress role) |
| `web_search` | read | Provider-backed search; configured via `[compute.web_search]` |

## Modality

| Tool | Risk | Description |
|---|---|---|
| `read_image` | read | Vision model on a local image path |
| `read_audio` | read | Audio transcription on a local path |
| `read_pdf` | read | PDF text/image extraction |

Each routes to a provider with the right capability — see [Providers](/configuration/providers).

## Memory

| Tool | Risk | Description |
|---|---|---|
| `memory_recall` | read | Top-k semantic search over episodic memory |
| `memory_forget` | mutating | Soft-delete records matching a query |
| `memory_adjudicate` | mutating | Resolve a conflict between two recorded claims |

## Soul

| Tool | Risk | Description |
|---|---|---|
| `soul_list` | read | List active soul fragments |
| `soul_tune` | mutating | Set/unset a soul fragment |
| `soul_history` | read | Past values of a fragment |

Sensitive — operator-only by default. See [Memory](/features/memory).

## Scheduling

| Tool | Risk | Description |
|---|---|---|
| `schedule_create` | reversible | Cron-style recurring task |
| `schedule_pause` | reversible | Pause a scheduled task |
| `schedule_resume` | reversible | Resume a paused task |
| `schedule_cancel` | mutating | Cancel a task |
| `schedule_list` | read | List active tasks |

## Commitments

| Tool | Risk | Description |
|---|---|---|
| `commitment_create` | reversible | One-shot future agent turn |
| `commitment_cancel` | reversible | Cancel before fire |
| `commitment_list` | read | Active commitments |
| `commitment_get` | read | Specific commitment by id |

## Council + research

| Tool | Risk | Description |
|---|---|---|
| `list_providers` | read | LLM providers + capabilities |
| `council_review` | read | Multi-provider parallel review |
| `research_start` | reversible | Async planner→workers→synth |
| `research_cancel` | reversible | Cancel an in-flight research task |

## OAuth + credentials

| Tool | Risk | Description |
|---|---|---|
| `oauth_start` | mutating | Begin device flow |
| `oauth_status` | read | Pending / completed flows |
| `oauth_cancel` | mutating | Cancel a pending flow |
| `credentials_list` | read | List stored credentials |
| `credentials_grant` | mutating | Grant a skill access to a credential |
| `credentials_revoke` | mutating | Revoke a grant |
| `credentials_delete` | destructive | Permanently delete a credential |

Sensitive — operator-only by default. See [OAuth and credentials](/security/oauth-and-credentials).

## ClawHub + skills

| Tool | Risk | Description |
|---|---|---|
| `clawhub_install` | mutating | Install a clawhub bundle (gated by `[security] clawhub_base_url`) |
| `mcp_add` | mutating | Add a configuration entry for a new MCP server |
| `mcp_list` | read | List wired MCP servers |

## Shell

| Tool | Risk | Description |
|---|---|---|
| `shell_command` | comm./destructive | Execute a shell command in the workspace mount |

Default-deny. Open with extreme care; this is the most prompt-injection-vulnerable surface.

## Naming convention

`<noun>_<verb>` — `commitment_create`, `memory_recall`, `schedule_cancel`. Skill tools follow `<skill>.<tool>` (`gws-workspace.gmail.send`). MCP tools follow `<server>.<tool>` (`minimax.text_to_image`).

## Reference

- `internal/compute/builtins.go` — registry + helpers
- `internal/compute/builtin_*.go` — one file per logical group
