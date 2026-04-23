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

## Conventions

- **Diagrams stay in sync with code** ([decision `lobslaw-documentation-diagrams`](../../)). If you change a flow documented with mermaid, update the diagram in the same commit.
- **Audience split** ([decision `lobslaw-documentation-audiences`](../../)). Dev docs describe *how it works*. User docs describe *how to use it*. Keep them separate.
- **Architectural decisions** go in aide (`./.aide/bin/aide decision set ...`), not freeform markdown. Subsystem docs link to the decision by topic.

## Contributing

*(TODO — this section lands with Phase 12 polish. For now: create a feature branch, keep commits small and topical, respect the conventions above.)*
