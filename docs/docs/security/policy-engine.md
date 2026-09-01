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

Before the gate asks, the command is **classified** by what it does. A command line is split into
its steps — across `&&`, `||`, `;`, `|` and newlines — each step's program is looked up in a
shipped table, and the command's tier is the most severe tier any step reaches:

| Tier | Meaning | Examples |
|---|---|---|
| `read` | inspects state and changes nothing | `uname -a`, `git status`, `df -h /`, `cat /proc/self/status` |
| `write` | mutates local state, recoverably | `touch /tmp/probe`, `git commit`, `sed -i`, `rm /tmp/build/x` |
| `network` | reaches off the box | `curl`, `wget`, `ssh`, `git push`, `apt-get install` |
| `destructive` | deletes, changes the machine, or runs as root | `rm -rf /etc/hosts`, `systemctl restart`, `sudo -n true`, `chmod -R 777 /etc` |
| `unknown` | the classifier cannot read it | a `for` loop, `$(…)`, `` ` ` ``, `$CMD status`, `sh -c …`, any program not in the table |

The classification is **fail-closed**: a program the table does not name is `unknown`, an argument
whose value cannot be seen is `unknown`, and `unknown` runs unasked in no mode at all.

**What a command is pointed at counts, not only what it is called.** `rm /tmp/probe.txt`,
`rm -rf /` and `rm -rf $DIR` are one program and three different operations, so the operands of a
targeting program are read as paths:

- a path under a declared scratch root lowers a deletion to `write` — `rm -rf /tmp/build/out` is
  a write, `rm -rf /etc` is not;
- a path under `/etc`, `/usr`, `/bin`, `/boot`, `/` and friends raises the tier to `destructive`
  whatever the program — `cp payload /usr/bin/ls` is not a copy, and `echo x > /etc/passwd` is not
  an echo;
- a path whose value cannot be seen makes the whole command `unknown` — `rm -rf $DIR` is not
  `rm -rf /tmp/x` and is never treated as though it were.

Scratch roots default to `/tmp` and `/var/tmp`. Nothing else is assumed: a deployment with a
mounted workspace names it in `[compute.shell_approval] scratch_paths`. Note that this reads the
path as *written* — a symlink out of a scratch root defeats it — which is why the de-escalation
only ever turns a deletion into a write, and `write` still asks under the shipped default.

Check any command without running it:

```console
$ lobslaw policy classify 'echo start; touch /tmp/.w && rm /workspace/.w'
destructive · `rm /workspace/.w` (step 3 of 3) · echo, touch, rm

steps:
   1  read         reads_only                         echo start
   2  write        mutates_files                      touch /tmp/.w
   3  destructive  deletes_or_changes_machine_state   rm /workspace/.w

under each approval mode:
    strict    asks
  * standard  asks
    trusted   asks
```

### Approval modes

`[compute] approval_mode` decides which tiers are worth asking about:

| Mode | Runs without asking | Still asks about |
|---|---|---|
| `strict` | nothing | everything |
| `standard` *(default)* | `read` | `write`, `network`, `destructive`, `unknown` |
| `trusted` | `read`, `write` | `network`, `destructive`, `unknown` |

There is no mode that waves through `network`, `destructive` or `unknown`, and no configuration
adds one — an operator who wants that writes a policy rule and owns it.

A mode installs ordinary policy rules at the lowest priority the type allows, so anything an
operator wrote and anything an approval minted still outranks them, and the whole posture is
visible in `lobslaw policy list` rather than being invisible behaviour in Go. `strict` installs
nothing at all: it is the absence of those rules.

The default is `standard` rather than `strict` deliberately. A gate that asks eight times in four
minutes is answered by reflex, and a reflex is not consent — removing the questions nobody needed
to be asked is what makes the remaining ones legible.

The [hardline floor](/security/hardline-floor) is unaffected by every mode.

### A second opinion on commands that cannot be read

A probing agent writes exactly the shapes static reading refuses — loops, substitutions, variables
in the command slot — so the commands producing the most confirmations are the ones the classifier
helps with least. A model can read them:

```toml
[compute.roles]
command_risk = "big-model"        # a [[compute.providers]] label

[compute.shell_approval]
verdict_trust = "resolve_unknown"  # advisory (default) | resolve_unknown
```

It answers the **same closed enum** the classifier uses, and nothing else: no prose ever reaches
the prompt, and a reply that is unparseable, outside the enum, late, or marked low-confidence is
discarded with the static verdict left standing. What it is allowed to move is the security-
relevant part:

- `advisory` — it may only **raise** a tier. A wrong answer costs one confirmation nobody needed
  to give.
- `resolve_unknown` — it may additionally resolve `unknown` **down** to a concrete tier, and only
  `unknown`. Where the static classifier has an opinion it keeps it.

`resolve_unknown` is what stops a shell-loop probe asking every time. It is opt-in because the
command text is attacker-influenced, and a command that can argue its own tier down is the whole
vulnerability. There is no fallback to the main model: with no `command_risk` role assigned, no
model is consulted at all.

### Granting a whole tier for one conversation

Alongside *Approve* and *Approve for this chat*, a confirmation offers **Allow read-only here** /
**Allow local writes here** when the command classifies into one of those two tiers.

This exists for the case the per-command buttons cannot help with. An agent probing its
environment writes a different command every time, and those commands are compound, so no grant
can name them — the user is offered exactly one answer, *Approve*, over and over. A tier is
nameable even when the command is not.

The grant is recorded like any session grant, scoped to the conversation, under the reserved
resource `(risk=read)` or `(risk=write)`. It is never offered for `network`, `destructive` or
`unknown`: "allow everything I could not read" is not a decision anybody should be able to make
with one tap.

### Commands with no stable form

Some commands have no stable identity and are asked about **every time**, with no *per-command*
scope button offered (the tier button above may still be): anything containing a pipe, `&&`, `;`, a redirect, `$`, backticks, a backslash, a glob, a
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
| `shell:run` | the exact command (`git status --short`), or `(risk=read)` for a tier grant | Every `shell_command` call, asked separately from `tool:exec` |
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
