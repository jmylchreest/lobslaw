---
sidebar_position: 6
---

# Remote hosts

A **remote** is a machine that is not this one. `remote_ssh` runs a command there over SSH and returns the output.

The tool is **disabled by default**. See [Turning it on](#turning-it-on).

It exists because of where the agent lives. The lobslaw process holds the model keys, the channel tokens and, on a cluster, the mTLS material for the whole Raft group. Compiling a project next to all of that — or running anything the agent just wrote — is the problem. `shell_command` is for small local work and denies `ssh` outright for the same reason: an agent that can reach a shell that can reach another host has a second hop nothing in the tool layer can see or gate.

`remote_ssh` is that hop, made first-class.

## What makes it different from `shell_command` with `ssh`

**The model names a remote, never a host.** A call supplies `remote: "go"`. It cannot supply a hostname, a port, a user or a key, and the tool schema constrains `remote` to an enum of what you configured. The set of reachable machines is your config, and nothing the model emits can widen it.

That is the same rule identity already follows in this codebase: a value the model can choose is a *request*, not a *fact*, and the two must not share a channel.

**Every call is a tool call.** It carries a risk tier, goes through the policy engine, and lands in the audit log with the command it ran. An `ssh` invocation smuggled through a shell tool has none of that — the audit record says `shell_command`, and what actually ran is a string inside it.

**Host keys are verified.** Trust-on-first-use against a `known_hosts` file you nominate: an unknown host is recorded, and a host whose key *changed* is refused. TOFU is weak exactly once and strong every time after; disabled verification is weak forever, and the difference matters when the agent's whole purpose on that connection is to run code.

## Configuration

```toml
[[remote]]
name        = "go"
description = "Go toolchain, opencode, aide"
host        = "devbox-go.lobslaw-dev.svc.cluster.local"
port        = 2222
user        = "dev"
key_ref     = "file:/etc/lobslaw/remote/id_ed25519"
known_hosts = "/var/lobslaw/data/remote_known_hosts"

[[remote]]
name        = "rust"
description = "Rust toolchain, cargo, clippy"
host        = "devbox-rust.lobslaw-dev.svc.cluster.local"
key_ref     = "file:/etc/lobslaw/remote/id_ed25519"
known_hosts = "/var/lobslaw/data/remote_known_hosts"
```

| key | required | default | notes |
|---|---|---|---|
| `name` | ✓ | — | What the agent passes. Short and about the stack; it is what the model reasons with, and a hostname is not. |
| `description` | | — | Reaches the model in the tool schema, so it is how the agent picks between remotes. Say what the toolchain is. |
| `host` | ✓ | — | |
| `port` | | `2222` | Not 22: a devbox sshd running unprivileged under a restricted PodSecurity policy cannot bind a privileged port. |
| `user` | | `dev` | |
| `key_ref` | ✓ | — | `file:` or `env:`. Prefer `file:` — an env var round-trips the newlines through whatever wrote it, and the failure surfaces at first dial rather than at boot. |
| `key_passphrase_ref` | | — | For an encrypted key. |
| `known_hosts` | | — | A path, not a secret ref: it is written as well as read. Empty pins for the process lifetime rather than disabling verification. |
| `default_timeout_secs` | | `300` | Builds are slow; this is minutes where `shell_command` allows seconds. |
| `max_timeout_secs` | | `3600` | Ceiling a call may ask for. |

No `[[remote]]` blocks leaves `remote_ssh` unregistered — a node with nowhere to dispatch to should not advertise a dispatcher. A block that is *present and broken* fails boot instead, because an unreadable key is a configuration error you want at start-up, not a tool error surfacing mid-turn that reads like a model fault.

## Turning it on

`remote_*` is **disabled by default**, at registration rather than by policy: the tool is never added to the registry, so the agent does not see it and cannot call it.

That is a stronger gate than a policy deny, which still puts the tool in the model's list and then refuses each call — the agent keeps trying, and the user reads the refusals as the agent being broken.

```toml
[compute]
disabled_tools = []              # nothing disabled, including remote_*
```

`disabled_tools` is a list of globs matched against tool names, and it distinguishes *unset* from *empty*:

| value | effect |
|---|---|
| *(absent)* | `["remote_*"]` — the default |
| `[]` | nothing disabled |
| `["remote_scp"]` | `remote_ssh` on, `remote_scp` off |
| `["remote_*", "shell_command"]` | the family, plus the local shell |

It applies to every source of tools, not just builtins — a skill manifest or an MCP server declaring a `remote_*` tool is suppressed by the same list. A malformed glob matches *nothing*: a typo should leave you with a tool still visible and a pattern to fix, not a node that looks broken.

## Policy

`remote_ssh` is in `noSeedTools`: it gets neither a default-allow nor a default-deny. Builtins are seeded default-allow because they are lobslaw-curated with a well-understood blast radius. Here the blast radius is whatever the agent decides to type, on a host that usually holds a git push token. That the devbox is disposable bounds the damage; it does not make the decision ours to make for you.

```toml
[[policy.rules]]
id       = "owner-devbox"
subject  = "scope:owner"
action   = "tool:exec"
resource = "remote_ssh"
effect   = "allow"
priority = 20
```

## What a call returns

```json
{
  "remote": "go",
  "command": "go test ./...",
  "exit_code": 1,
  "stdout": "...",
  "stderr": "--- FAIL: TestThing\n",
  "duration": "12.4s"
}
```

**A non-zero `exit_code` is a result, not a failure.** A failing build is the answer to "does this build", and the tool returns it as one. The error path is reserved for the transport — unreachable, refused, timed out — because the agent's next move differs completely between "the compiler disagreed" and "the machine is not there".

Output is capped at 256 KiB per stream, keeping the *first* bytes and setting `truncated`. First rather than last: a compiler reports what went wrong at the top and then repeats itself.

## Sandboxing

There isn't any, here, and that is the design. The remote *is* the boundary — a separate machine, a separate network policy, a workspace you can throw away. Applying lobslaw's Landlock/seccomp sandbox to an SSH connection would sandbox the connection, not the command, which buys nothing and would imply a protection that does not exist.

What bounds the damage is what you give the remote: no route to anything that matters, a push token scoped to the repos it should touch, and a workspace whose loss costs a `git push`.

## Working with an agent on the remote

`remote_ssh` runs a command; it does not care what the command is. The pattern that makes this worth having is running a *coding agent* there — `opencode run`, or a headless `opencode serve` — so edits happen with a real edit/build/test loop, tool-call logging and memory, on the machine that has the toolchain.

The prose that teaches an agent to work this way is a skill, not a tool. See the `devbox-dispatch` skill.
