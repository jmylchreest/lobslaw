# lobslaw — Developer Docs

For people modifying lobslaw itself. End-user docs live in [`../user/`](../user/).

## Start here

- [**ARCHITECTURE.md**](ARCHITECTURE.md) — the overarching component diagram (C4 container level) and the landmarks every contributor needs to know.

## Subsystem docs

| Area | Doc | Covers |
|---|---|---|
| Cluster / Raft | *(tbd — MEMORY.md covers storage-side for now)* | Node startup, membership, Raft transport |
| Discovery | [DISCOVERY.md](DISCOVERY.md) | Seed list, DNS expansion, UDP broadcast |
| Memory | [MEMORY.md](MEMORY.md) | Store, Recall, Search, FindClusters, Forget, Dream + merge flow, Adjudicator |
| Sandbox | [SANDBOX.md](SANDBOX.md) | Namespaces, Landlock, seccomp, reexec helper, policy.d |
| Policy engine | *(tbd — linked from MEMORY.md + SANDBOX.md for now)* | Rule walk, conditions, evaluator injection |
| Executor | *(tbd)* | Tool invocation pipeline, env whitelist, capped output |
| Agent loop | [AGENT.md](AGENT.md) | RunToolCallLoop, resolver, promptgen, LLM client, budget |
| Gateway (channels) | [GATEWAY.md](GATEWAY.md) | REST server, Telegram webhook, confirmation prompts, JWT validator |
| Scheduler | [SCHEDULER.md](SCHEDULER.md) | Sleep-until-due loop, CAS claim, PlanService, built-in agent:turn handler |
| Storage | [STORAGE.md](STORAGE.md) | Mount Manager + Watcher, local/nfs/rclone backends, StorageService gRPC |
| Skills | [SKILLS.md](SKILLS.md) | Manifest-driven user skills, registry + invoker (python/bash), sandbox integration pending |

## Conventions

- **Diagrams stay in sync with code** ([decision `lobslaw-documentation-diagrams`](../../)). If you change a flow documented with mermaid, update the diagram in the same commit.
- **Audience split** ([decision `lobslaw-documentation-audiences`](../../)). Dev docs describe *how it works*. User docs describe *how to use it*. Keep them separate.
- **Architectural decisions** go in aide (`./.aide/bin/aide decision set ...`), not freeform markdown. Subsystem docs link to the decision by topic.

## Shared aide context

`.aide/shared/` holds decisions (and, if you enable it, memories) exported as
git-friendly markdown — one file per record, so re-exporting unchanged content
is byte-identical and deletions propagate as tombstones. It is committed. The
BoltDB store it comes from (`.aide/memory/`) is gitignored and machine-local,
which means CI cannot regenerate it and git has to move it for us.

Hooks in [`.githooks/`](../../.githooks/) do that, both directions:

| Hook | Does |
|---|---|
| `pre-commit` | `aide share export`, then stages `.aide/shared` |
| `post-merge` | `aide share import` — records that arrived with a merge or `git pull` |
| `post-rewrite` | same, for `git pull --rebase` (which never fires `post-merge`) |
| `post-checkout` | same, after a branch switch or the initial checkout of a clone |

Install them once per checkout:

```bash
make hooks     # installs betterleaks, then git config core.hooksPath .githooks
```

Git deliberately never installs hooks from a clone, so this step cannot be
automated by the repo. Scope is policy-driven, not hardcoded in the hooks —
`./.aide/bin/aide share show` prints what would be published. The hooks no-op
when there is no aide binary or no local store; `AIDE_HOOKS=0` disables them and
`git commit --no-verify` skips the export.

### Secret scanning — the hook expects betterleaks installed

`pre-commit` also runs [betterleaks](https://github.com/betterleaks/betterleaks)
over the **staged** diff and fails the commit on a hit. `make hooks` installs it;
`make hook-tools` installs it alone. Unlike the protoc-gen-* tools it is
installed at `@latest` rather than pinned — a scanner is only as good as its
newest rules, and pinning one freezes detection at whenever someone last chose
a number. Without it the hook **fails rather than skipping** — a secret scanner
that silently no-ops is worse than none, because it stops anyone checking by
hand. `LOBSLAW_SKIP_SECRET_SCAN=1` is the one-off escape.

Two details worth knowing:

- It scans the staged diff, not the working tree. Real credentials do live in
  this checkout — `deploy/docker/.env`, `deploy/docker/secrets/`, the aide
  BoltDB store under `.aide/memory/` — and they are correctly gitignored.
  Scanning the tree would flag them on every commit, and a gate that cries wolf
  is one everybody learns to bypass.
- Findings are reported `--redact`ed: file and line, never the value. Echoing it
  would copy the secret into terminal scrollback, shell history, and any CI log
  that captures the output.

CI runs the same scanner over the full history in [`lint.yml`](../../.github/workflows/lint.yml),
so `--no-verify` delays the check rather than skipping it.

The scanner matters most for `.aide/shared/`: it is committed, this repository
is public, and the export above is automatic — so a credential an agent writes
into a decision mid-session is published on the next push, with no human having
read that diff. Everything else in a commit, you wrote and reviewed.

## Contributing

*(TODO — this section lands with Phase 12 polish. For now: create a feature branch, keep commits small and topical, respect the conventions above.)*
