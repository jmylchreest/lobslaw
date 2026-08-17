---
sidebar_position: 1
---

# CLI

The `lobslaw` binary is multi-mode — the same binary handles run, init, doctor, plugin install, cert signing, sandbox-exec helper.

## Subcommand list

```
lobslaw                    # run the node (with --config)
lobslaw init               # interactive config scaffold
lobslaw doctor             # config + connectivity checks
lobslaw context            # the clusters this machine can reach
  context list             # show the configured contexts
lobslaw nodeid             # derive a deterministic node ID for this host
lobslaw cluster            # cluster + cert lifecycle
  cluster ca-init          # create cluster CA
  cluster sign-node        # sign a node cert against the CA
  cluster sign-operator    # sign an OPERATOR cert — administers, cannot join
  cluster reset            # nuke the local raft state (DESTRUCTIVE)
lobslaw plugin             # plugin lifecycle
  plugin install <bundle>  # install a clawhub bundle
  plugin list              # list installed skills
lobslaw audit              # the tamper-evident record
  audit query              # read entries
  audit verify             # walk the hash chain
lobslaw policy             # see and undo "always" approvals
  policy approvals         # list the rules an approval minted
  policy revoke-approvals  # delete them, all or by id
lobslaw memory             # read + edit the memory store
  memory show <id>         # one record in full
  memory list              # list vector + episodic records
  memory forget            # delete records and their consolidations
  memory share <id>...     # make owned records readable cluster-wide (offline)
  memory unshare <id>...   # return shared records to their owner only (offline)
  memory consolidations    # what Dream merged, superseded or left alone (offline)
lobslaw session            # read conversation transcripts
  session list             # one line per conversation
  session show <id>        # full transcript
  session search <text>    # substring search across transcripts
lobslaw sandbox-exec       # hidden — used by the sandbox reexec helper
lobslaw dispatch           # hidden — used by hooks / scheduler dispatch
```

## Global flags

```
--config <path>            # config.toml path (required for run, doctor)
--context <name>           # named cluster from contexts.toml (see below)
--log-level <debug|info|warn|error>
--log-format <text|json>
```

`--context` and `--config` answer different questions. `--config` is the
node's file: where its data lives, which certs it presents, what it binds.
`--context` is the operator's: which remote cluster to talk to and what
credential to present. A laptop administering a cluster needs the second and
should not have the first.

## `lobslaw` (no subcommand)

Runs the node:

```bash
lobslaw --config /etc/lobslaw/config.toml
```

Foreground process. SIGTERM for graceful shutdown, SIGHUP for config reload + cert reload.

## `lobslaw init`

Interactive scaffold — walks through prompts, writes `config.toml`, `.env`, `data/`, `audit/`, `certs/`. See [Getting Started → From Source](/getting-started/from-source) for the full walkthrough.

## `lobslaw doctor`

Runs every health check. See [Doctor](/operating/doctor) for the check list.

```bash
lobslaw doctor --config config.toml
```

## `lobslaw cluster ca-init`

```bash
lobslaw cluster ca-init \
  --ca-cert certs/ca.pem \
  --ca-key  certs/ca-key.pem
```

Generates a fresh ed25519 CA. One-time, per cluster.

## `lobslaw cluster sign-node`

```bash
lobslaw cluster sign-node \
  --ca-cert  certs/ca.pem \
  --ca-key   certs/ca-key.pem \
  --node-cert certs/node.pem \
  --node-key  certs/node-key.pem \
  --node-id  $(lobslaw nodeid)
```

Signs a node keypair against the CA. CN = node ID, SAN = `<id>` + `<id>.cluster.local`.

## `lobslaw cluster sign-operator`

```bash
lobslaw cluster sign-operator alice \
  --ca-cert certs/ca.pem \
  --ca-key  certs/ca-key.pem \
  --out     ./alice
```

Signs a credential for a PERSON rather than a host. CN is the operator's name,
so audit entries say who rather than which machine.

It is deliberately not a node certificate:

- **Client authentication only.** A node cert carries `ServerAuth` too, because
  a node both dials its peers and serves them — which is what made handing one
  to a laptop equivalent to handing over a cluster membership. Nothing can
  serve with an operator cert.
- **`OU=operator`, refused on the raft transport.** ClientAuth alone would
  still permit opening a raft stream, since a peer dials as a client too. The
  server rejects that OU on the peer-only paths, on the streaming interceptor
  as well as the unary one.
- **Shorter-lived by default** — 90 days. A person's credential travels.

Revoking one does not require rotating any node's identity. The command writes
`operator.pem`, `operator-key.pem` and a copy of `ca.pem` into `--out`, and
prints the `contexts.toml` block for them.

## `lobslaw context`

Named clusters, so administering a remote cluster does not mean four flags on
every invocation.

```bash
lobslaw context list          # what this machine can reach, and whether the files are there
```

The file lives at `$XDG_CONFIG_HOME/lobslaw/contexts.toml` (falling back to
`~/.config/lobslaw/contexts.toml`); `LOBSLAW_CONTEXTS` overrides the path.

```toml
default = "prod"

[contexts.prod]
addr    = "node1.example.com:9090"
ca_cert = "~/.config/lobslaw/prod/ca.pem"
cert    = "~/.config/lobslaw/prod/operator.pem"
key     = "~/.config/lobslaw/prod/operator-key.pem"

[contexts.staging]
addr    = "staging.example.com:9090"
ca_cert = "~/.config/lobslaw/staging/ca.pem"
cert    = "~/.config/lobslaw/staging/operator.pem"
key     = "~/.config/lobslaw/staging/operator-key.pem"
```

```bash
lobslaw --context staging memory list
LOBSLAW_CONTEXT=prod lobslaw memory list    # for a shell that lives in one cluster
```

`default` is optional. Leaving it out is reasonable when you have both a
staging and a production cluster: a bare command then refuses rather than
picking one.

**Precedence**, highest first:

1. Explicit `--addr` / `--ca-cert` / `--node-cert` / `--node-key`. Overriding
   one keeps the rest of the context — you do not have to supply all four to
   change the address.
2. The named context (`--context`, `LOBSLAW_CONTEXT`, or `default`).
3. `--config`, reading `[cluster] advertise_addr` and `[cluster.mtls]`.

A context outranks `config.toml` because a `config.toml` found on a laptop is
likelier to be left over from running a node locally than the cluster you meant
to reach.

An unknown context name is an error listing what exists, not a fall back to the
default. The failure worth preventing is a command aimed at staging that lands
on production.

## `lobslaw plugin install <bundle>`

```bash
# from clawhub
lobslaw plugin install clawhub:gws-workspace@1.0.0

# from local directory
lobslaw plugin install file:///path/to/manifest-dir/

# from a git repo (planned)
lobslaw plugin install git://github.com/owner/skill@v1.0.0
```

Honours `[security] clawhub_signing_policy`.

## `lobslaw audit`

The tamper-evident record of what the agent was permitted to do.

```bash
lobslaw audit query --context prod --since 24h --action tool:exec
lobslaw audit verify --context prod
```

Both subcommands talk to a **running node** by default. Reading the log only
from the local filesystem made it the record of what *this machine* was
permitted to do — which on a laptop is nothing at all, and an empty audit log
reads as a quiet cluster rather than as the wrong file.

`--offline` is the opt-out, and it is the forensic path: a node that will not
start still has its `audit.jsonl`, and that is exactly when somebody wants to
read it.

```bash
lobslaw audit verify --offline --path /var/lib/lobslaw/audit/audit.jsonl
lobslaw audit query  --offline --config config.toml --actor user:alice
```

**Filters** (both forms): `--actor`, `--action`, `--target`, `--since`,
`--until`, `--limit`. `--since` and `--until` take an RFC3339 instant *or* a
bare duration meaning "ago" — `--since 24h`. A window that ends before it
starts is refused rather than returning nothing, because an empty result from
a backwards window looks exactly like an empty result from a quiet cluster.

**`--sink`** picks `raft` or `local` on a running node. Omitted, `verify`
checks every sink **separately** and names any that breaks — the service
flattens a combined check into one verdict, and "the chain is broken" without
saying which sink is half an answer.

`verify` exits non-zero on a break. It also refuses when *no* sink could be
checked: exiting 0 having verified nothing is the failure this command exists
to catch. A sink the node does not run is reported as unavailable, not as a
broken chain — conflating the two sends somebody looking for tampering that
did not happen.

## `lobslaw policy`

An "always" approval is a permanent widening of what the agent may do — tapped
once, then easy to forget. These are the other half of that feature: without a
way to see and undo the grants, "revocable" is a claim in a doc rather than
something a person can act on.

```bash
lobslaw policy approvals --context prod
lobslaw policy revoke-approvals --context prod approval:abc123 --apply
lobslaw policy revoke-approvals --context prod --all --apply
```

Live by default; `--offline` opens `state.db` directly and needs the node
**stopped**, because bbolt takes an exclusive lock. Both forms print where they
read from — an empty list of grants is indistinguishable from the wrong store
unless the source is on the page.

`revoke-approvals` is a **dry run unless `--apply`** is given, and naming
nothing is not "everything": pass `--all` explicitly, or the command refuses.

**The scope is enforced by the node, not by the CLI.** `RevokeApprovalRules` is
scoped to approval-minted rules on the server, so the guarantee an operator
relies on — that revoking their approvals cannot touch a rule they wrote by
hand — does not depend on which client made the call. There is deliberately no
unscoped delete RPC. A rule that exists but was not minted by an approval is
reported as *refused*; an id that does not exist at all is reported as *not
found*. Those are different mistakes with different fixes.

## `lobslaw memory` and `lobslaw session`

### `memory show`, `list` and `forget` talk to a running node

```bash
lobslaw memory list --context prod --kind episodic --tag meeting
lobslaw memory show --context prod <id>
lobslaw memory forget --context prod --tag scratch --apply
```

Reading the store only from the local filesystem made these the record of what
*this machine* remembers — which on a laptop is nothing, and an empty listing
reads as an empty cluster. Live is the default; `--offline` opens `state.db`
directly and needs the node stopped (see below).

`memory share`, `unshare` and `consolidations` have **no live form yet**. They
still run, and they say so on stderr rather than quietly reading a local
`state.db` that is not the cluster's.

`forget` is a **dry run unless `--apply`**, in both forms. It is irreversible
and it cascades: a consolidation whose sources are deleted is deleted too,
because keeping a summary of removed records leaks the removed content through
the summary's own text and embedding. The dry run names both sets, plus any
requested id that does not exist — for a hand-typed id, "no such record" is
nearly always a typo, and a forget that quietly deletes nothing is the worst
outcome.

`--requester <principal>` runs the forget on somebody's behalf: records that
principal may not read are left alone, and — crucially — they are dropped
*before* the cascade runs, so they cannot pull their consolidations down with
them. Omitting it is the operator's unrestricted view.

Filters: `--kind all|vector|episodic`, `--owner`, `--scope` (vector only),
`--tag` (episodic only), `--unowned`, `--limit`. `--scope` and `--tag` exclude
the other kind outright rather than returning it unfiltered. `--unowned` is not
the same as an empty `--owner`: it selects records belonging to no principal at
all. A truncated listing reports the pre-limit totals, so "20 of 400" never
reads as 400.

### `session list`, `show` and `search` talk to a running node

```bash
lobslaw session list   --context prod --channel telegram
lobslaw session show   --context prod <id>
lobslaw session search --context prod "the pelican brief"
```

Live by default, `--offline` to open `state.db` directly. Read-only in both
forms: forgetting a conversation is a replicated mutation with its own path,
and browsing what was said does not need one that can also delete it.

`search` goes through the same `SearchTranscripts` the agent's `session_search`
tool uses, so what an operator sees is what the model would have found. The
match count reported per conversation is the **total**, which may exceed the
snippets shown — that is what tells a passing mention from a thread about the
thing. An empty search is refused rather than enumerating everything; use
`session list` for that.

### Stop the node first — for the offline path

`--offline` opens the node's `state.db` directly. bbolt takes an **exclusive
file lock**, so a running node makes it fail after a five-second wait with:

```
state.db at /var/lib/lobslaw/data/state.db is locked by another process —
the node is running; stop it first.
```

There is no read-only mode that gets around this. Stop the node, run the
command, start it again.

### Locating the store

The offline path in both groups accepts the same four flags:

```
--config <path>          # reads [cluster] data_dir and [memory.encryption] key_ref
--data-dir <path>        # data dir holding state.db; overrides --config
--state-db <path>        # explicit file path; overrides --data-dir and --config
--memory-key-ref <ref>   # env:VAR | file:/path; overrides the config's key_ref
```

The encryption key is resolved in that order and falls back to
`$LOBSLAW_MEMORY_KEY`. A `.env` next to `config.toml` is loaded first, so
the key that starts the node normally works here without re-exporting it.

Every subcommand also takes `--json` for scripting.

### `lobslaw memory list`

```bash
lobslaw memory list --config config.toml --kind episodic --limit 20
```

Filters: `--kind all|vector|episodic`, `--owner`, `--scope` (vector),
`--tag` (episodic), `--unowned`, `--limit`. Newest first.

Records with no owner are marked `!` and counted in the footer. Ownership is
stamped on every record written since it existed, so an unowned record today
is an anomaly worth chasing rather than a normal state — it belongs to no
principal, and `share` / `unshare` refuse to touch it.

### `lobslaw memory show <id>`

Full field dump for one vector or episodic record, plus the list of
consolidations that name it in their `source_ids` — i.e. exactly what a
`forget` of this record would take down with it. The raw embedding is
reported as a dimension count rather than printed.

### `lobslaw memory forget`

```bash
# dry run — prints what would go
lobslaw memory forget --config config.toml --query "old project"

# actually delete
lobslaw memory forget --config config.toml --query "old project" --apply
```

Filters: `--id` (repeatable), `--query`, `--before` (RFC3339 or `YYYY-MM-DD`),
`--tag` (repeatable). At least one is required — an unfiltered forget matches
every record in the store and is refused.

Forget **cascades**: any consolidation whose `source_ids` intersect the
matched set is swept too. Keeping a summary whose sources were deleted would
leak the deleted content through the summary's own text and embedding, so the
cascade is the point of the operation rather than a side effect. The dry run
reports the matched set and the cascade separately.

### `lobslaw memory share` / `lobslaw memory unshare`

```bash
lobslaw memory share --config config.toml <id> <id> --apply
```

Flips `visibility` between `SHARED` (readable by any authenticated principal)
and `PRIVATE` (readable by the owner). Refuses the whole batch if any id is
unknown or unowned.

### `lobslaw session list` / `show` / `search`

```bash
lobslaw session list --config config.toml
lobslaw session show --config config.toml telegram:-1001234567
lobslaw session search --config config.toml "connection refused"
```

`search` is a substring search over stored transcripts and drives the same
service the agent's `session_search` tool uses, so an operator sees what the
model would have found. Filters: `--channel`, `--user`, `--limit`,
`--snippets`.

`show` prints message text in full; `--truncate N` caps it.

### What is deliberately absent

There is no `memory add`. A memory needs an embedding to be findable and the
CLI has no embedder wired; operator-seeded knowledge is config-declared and
seeded at boot instead.

There is no `session forget`. Deleting a conversation is a replicated
operation and goes through the running node.

## `lobslaw sandbox-exec`

Hidden subcommand — invoked only by the sandbox reexec helper, never by the operator directly. Reads `LOBSLAW_SANDBOX_POLICY` env, installs NoNewPrivs + Landlock + seccomp, then `execve`s the target. See [Sandbox](/security/sandbox).
