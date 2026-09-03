---
sidebar_position: 4
---

# Hardline Floor

The refusals no configuration can turn off.

## Why it exists

The [policy engine](/security/policy-engine) is default-deny, which is right — but it is
operator-configurable all the way down. A single rule does it:

```toml
[[policy.rule]]
subject = "*"
action = "*"
resource = "*"
effect = "allow"
```

With that in place there is nothing between a persuaded model and `rm -rf /`. Every real
deployment eventually grows a rule broader than intended, and confirmations get switched off by
whoever is tired of tapping Approve.

The hardline floor is the set of things that stay refused anyway. It is compiled into the binary,
reads no configuration, and has no override flag.

It is now the **only** compiled-in refusal on the shell path. `shell_command` used to carry a
substring denylist of its own — `sudo`, `ssh`, `curl`, `wget`, `dd` and others — which refused
outright with no way to say yes, so the only recourse was editing the source. Those commands go to
the [per-command approval gate](/security/policy-engine#per-command-shell-approval) instead, where
a human can answer. The floor did not move: it is checked before policy and before that gate, and
`ApprovalRules.Mint` refuses to write a rule for anything it denies, so no approval at any scope
reaches past it.

Nothing about **risk classification** reaches it either. `[compute] approval_mode` decides which
labels are worth asking a human about, `scratch_paths` decides where a deletion counts as a write,
and an optional model may offer a second opinion on commands the classifier cannot read — all of
which happen *after* the floor has already refused what it refuses. A command the floor denies is
never classified, never prompted about, and never granted, under any approval_mode — including one that names every label.

## What it refuses

**Commands** (`internal/policy/hardline.go`, `CheckCommand`):

| Pattern | Example |
|---|---|
| Filesystem wipes | `rm -rf /`, `rm -rf /*`, `rm --recursive --force /` |
| `--no-preserve-root` | any use — the flag exists only to remove the guard |
| Fork bombs | `:(){:|:&};:` and every renamed or reformatted variant |
| Block-device formatting | `mkfs.ext4 /dev/sda1` |
| Raw block writes | `dd if=… of=/dev/sda` |
| Network piped to an interpreter | `curl … \| sh`, `wget -O- … \| python3` |
| World-writable root | `chmod -R 777 /` |
| Recursive chown of `/` | `chown -R root:root /` |

**Paths** (`CheckPath`), independent of sandbox or mount configuration:

| Path | Verdict |
|---|---|
| `~/.ssh` | refused |
| `~/.ssh/config`, `~/.ssh/known_hosts` | confirmation — no key material, and refusing them breaks ordinary work |
| `~/.aws`, `~/.kube`, `~/.gnupg` | refused |
| `/etc/shadow`, `/etc/sudoers` | refused |
| `.env*`, `.envrc` | refused |
| `state.db*`, `*.key`, `*.pem` | refused — lobslaw's own state and key material |

## Where it runs

Three places, deliberately overlapping:

1. **`Executor.Invoke`, before policy rules load.** Running it after policy would mean an
   allow-everything rule reached it first. A floor evaluated after the thing it is a floor for is
   not a floor.
2. **The shell builtin's argument inspection.** `shellDenylist` beside it is the
   operator-facing layer and is expected to become relaxable by config; this check never is.
3. **The fs builtins** (`read_file`, `write_file`, `edit_file`), which render the refusal as a
   structured tool error.

A confirmation verdict is the one case evaluated *after* policy: a path the operator's rules
already deny is refused outright rather than prompting the user about something that was never
going to be permitted. The floor can raise the bar, never lower it.

## What it does not protect against

**This is not a security boundary, and treating it as one would be worse than not having it.**

A shell can reach every path on that list by other means — a here-doc, a base64'd script, an
interpreter, a path spelled in a way the pattern does not match. `CheckCommandPaths` scans
whitespace-separated words rather than parsing shell grammar, so quoting and expansion defeat it.

The real boundary is the [sandbox](/security/sandbox): Landlock, seccomp, and namespaces enforce
what a running process can touch, regardless of how it phrases the request.

What the floor is actually for:

- **Accidents.** A model that has misunderstood the task and reaches for `rm -rf /` gets stopped.
- **An unambiguous stop signal.** The refusal is a distinct error type, rendered to the model as
  "this is compiled in, there is no configuration that permits it, do not retry and do not look
  for another way". That reads differently from a policy denial, which invites seeking approval.
- **A guarantee that survives misconfiguration.** Whatever an operator does to the rules, this
  much still holds.

## Interaction with approvals

Neither a session grant ("approve for this chat") nor a permanent approval can reach past the
floor. A permanent approval is a policy allow rule, and the floor is evaluated before policy — so
the escalation path closes by construction rather than by call ordering. There is a test for it
(`TestSessionGrantCannotReachTheFloor`), because "by construction" is a claim, not a proof.

## Changing it

The pattern lists are the easy part. The property worth protecting is that no configuration
disables the floor — `TestHardlineTakesNoConfiguration` asserts the entry points take a single
string and nothing else, so a refactor that threads options through has to change that test on
purpose.

The false-positive table in `hardline_test.go` is as load-bearing as the refusal table. A floor
that refuses ordinary work gets switched off, and then it protects nothing.
