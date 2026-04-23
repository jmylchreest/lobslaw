# lobslaw

Cluster-wide, decentralised personal AI assistant. Privacy- and security-first.

The name is a portmanteau of *lobster* and *openclaw*, with a nod to *coleslaw*.

---

## What are you trying to do?

**I want to install, configure, or operate lobslaw.**
→ Start in [**docs/user/**](docs/user/).

**I want to understand how lobslaw is built or contribute to it.**
→ Start in [**docs/dev/**](docs/dev/). The [architecture diagram](docs/dev/ARCHITECTURE.md) is a good first read.

**I'm evaluating whether lobslaw fits my use case.**
→ Skim [docs/user/OVERVIEW.md](docs/user/OVERVIEW.md) for the user-facing story, then [docs/dev/ARCHITECTURE.md](docs/dev/ARCHITECTURE.md) for the shape.

---

## Project status

Phase 5 in progress. Foundation (Phases 1–4) complete: cluster core, memory service with Raft, policy engine, tool executor, sandbox (Linux namespaces + Landlock + seccomp via reexec helper), hook dispatcher. See [PLAN.md](PLAN.md) for the full roadmap.

## Documentation layout

- **[docs/user/](docs/user/)** — installation, configuration, channel setup, skills, troubleshooting. Assumes no knowledge of the internals.
- **[docs/dev/](docs/dev/)** — architecture, subsystem design, decisions, diagrams, contribution guide. Assumes you're working on lobslaw itself.

Both are maintained in sync with code per aide decisions [`lobslaw-documentation-audiences`](.aide/) and [`lobslaw-documentation-diagrams`](.aide/) — diagrams update in the same commit that changes the underlying flow.
