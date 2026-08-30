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
| `web_search` | read | Pluggable search backend (SearXNG, Exa, or any engine described in TOML) via the `web_search` egress role; configured via `[compute.web_search]` + `[[compute.search_providers]]`. See [Web search](/features/web-search) |

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
| `shell_command` | comm./destructive | Execute a shell command in the workspace mount, **on this machine** |

Default-deny. Open with extreme care; this is the most prompt-injection-vulnerable surface.

`ssh` and `scp` are on its hard denylist — see `remote_ssh` below for why that is a feature rather than a gap.

## Remote

| Tool | Risk | Description |
|---|---|---|
| `remote_ssh` | irreversible | Run a command on a configured host over SSH — see [Remote hosts](/configuration/remotes) |
| `remote_scp` | irreversible | Copy one file to/from a configured host. `local_path` gets the same guards as `read_file`/`write_file`, in the direction of travel |

**Disabled by default.** Configured via `[[remote]]`, enabled via `disabled_tools` below.

## Disabling tools

`compute.disabled_tools` is a list of glob patterns matched against tool **names**. A matching tool is never registered, so the agent does not see it in its tool list and cannot call it.

```toml
[compute]
disabled_tools = ["remote_*"]
```

### Why registration and not policy

A policy deny leaves the tool in the model's list and refuses each call. The agent keeps trying, the refusals accumulate, and the user reads it as the agent being broken rather than as a setting somebody chose. Not registering the tool means the question never arises.

The two gates answer different questions and both still apply:

| | question | mechanism |
|---|---|---|
| `disabled_tools` | does this tool exist on this node? | registration |
| `[[policy.rules]]` | may this caller use it? | policy engine |

A tool can be registered and denied. A tool that is disabled is not reachable by any rule, because there is nothing to write a rule about.

### Absent is not empty

| value | effect |
|---|---|
| *(absent)* | `["remote_*"]` — the default |
| `[]` | nothing disabled, including `remote_*` |
| `["remote_scp"]` | `remote_ssh` on, `remote_scp` off |
| `["remote_*", "shell_command"]` | the remote family, plus the local shell |

Deleting the key is **not** the same as setting it to `[]`. An absent key means "I have not decided" and takes the default; an empty list means "I have decided: all of them". `lobslaw init` writes the default out explicitly for exactly this reason — a default nobody can read is a default nobody revisits.

### It covers every source of tools

The gate sits on the tool registry, which is the only place builtins, skill manifests and MCP servers all pass through. A skill or an MCP server declaring a tool named `remote_ssh` is suppressed by the same list; gating in the builtin wiring would have covered the builtins and quietly missed those.

### A bad pattern matches nothing

`disabled_tools = ["[unclosed"]` disables nothing at all. The failure directions are not symmetric: a typo that matches nothing leaves you with a tool still visible and a pattern to fix, while a typo that matched everything would be a node with no tools and no obvious cause.

## Path guards

Every builtin that takes a filesystem path asks the same three questions, in this order:

1. **mount resolver** — does this path exist to the agent, in this mode? Declared by `[[storage.mounts]]`. Absoluteness falls out of this step, because the mount-label form (`workspace/notes.md`) is legitimately relative until it expands.
2. **hardline floor** — `policy.CheckPath`. Three verdicts: allow, **confirm**, deny. No configuration reaches it.
3. **`policy.d/<tool>.toml`** — may *this tool* touch it? Narrowing only.

Step 1 grants, step 2 refuses, step 3 subtracts. That `policy.d` can never widen is load-bearing rather than stylistic — see [the sandbox notes](https://github.com/jmylchreest/lobslaw/blob/main/docs/dev/SANDBOX.md).

`list_files` and `glob` return names rather than content: they run steps 1 and 2 on the directory, then filter individual entries against the floor rather than failing. `shell_command` has a command string rather than a path and tokenises it through `policy.CheckCommandPaths`, which refuses a *confirm* verdict outright because a shell has nowhere to ask.

## Naming convention

`<noun>_<verb>` — `commitment_create`, `memory_recall`, `schedule_cancel`. Skill tools follow `<skill>.<tool>` (`gws-workspace.gmail.send`). MCP tools follow `<server>.<tool>` (`minimax.text_to_image`).

## Reference

- `internal/compute/builtins.go` — registry + helpers
- `internal/compute/builtin_*.go` — one file per logical group
