# lobslaw — Architecture

High-level shape of the system. Start here, then dive into a subsystem doc.

## Component diagram (C4 container level)

```mermaid
flowchart TB
  subgraph UserFacing["User-Facing (Phase 6)"]
    REST["gateway.Server<br/>/v1/messages /healthz /readyz<br/>/v1/prompts/{id} /v1/prompts/{id}/resolve"]
    TG["gateway.TelegramHandler<br/>webhook + inline keyboard"]
    Prompts["gateway.PromptRegistry<br/>confirmation state<br/>(in-memory, TTL auto-deny)"]
    JWT["pkg/auth.Validator<br/>HS256 (RS256/EdDSA=TBD)"]
    REST --> JWT
    REST --> Prompts
    TG --> Prompts
  end

  subgraph Agent["Agent loop (Phase 5)"]
    AgentLoop["compute.Agent<br/>RunToolCallLoop"]
    Resolver["compute.Resolver<br/>pick provider chain"]
    Promptgen["promptgen<br/>build system prompt"]
    Budget["compute.Budget<br/>turn spend/tools"]
    LLMClient["compute.LLMClient<br/>OpenAI-compat /chat/completions"]
  end

  subgraph Compute["Tool execution (Phase 4)"]
    Registry["compute.Registry<br/>tools + per-tool policy"]
    Executor["compute.Executor<br/>invoke pipeline"]
    Policy["policy.Engine<br/>rule walk + conditions"]
    Hooks["hooks.Dispatcher<br/>PreToolUse / PostToolUse / etc"]
    Sandbox["sandbox<br/>Apply → reexec helper"]
  end

  subgraph LLMLayer["LLM interpretation (Phase 5+)"]
    Summarizer["Summarizer iface<br/>(Dream consolidation)"]
    Adjudicator["Adjudicator iface<br/>(merge verdict)"]
    Reranker["Reranker iface<br/>(hot-path recall filter)"]
  end

  subgraph Memory["Memory service (Phase 3)"]
    Store["memory.Store<br/>bbolt + Raft FSM"]
    MemSvc["memory.Service<br/>Store/Recall/Search/Forget/FindClusters"]
    Dream["memory.DreamRunner<br/>score + consolidate + merge phase"]
    Raft["etcd/raft/v3<br/>+ custom gRPC transport"]
  end

  subgraph Cluster["Cluster core (Phase 1-2)"]
    Discovery["discovery.Service<br/>seed + DNS + UDP broadcast"]
    MTLS["pkg/mtls<br/>per-node certs"]
    NodeSvc["node.Node<br/>lifecycle + gRPC server"]
  end

  REST --> AgentLoop
  TG --> AgentLoop
  AgentLoop --> Promptgen
  AgentLoop --> Resolver
  AgentLoop --> LLMClient
  AgentLoop --> Executor
  AgentLoop --> Budget
  AgentLoop --> MemSvc

  Resolver -.reads config.toml.-> Resolver
  LLMClient -- HTTPS --> ExternalLLM["External LLM<br/>(OpenAI, Anthropic, Ollama)"]

  Executor --> Registry
  Executor --> Policy
  Executor --> Hooks
  Executor --> Sandbox

  Sandbox -. reexec .-> SandboxExec["lobslaw sandbox-exec<br/>(helper subcommand)"]
  SandboxExec -- prctl + landlock + seccomp --> Tool["target tool subprocess"]

  Dream --> MemSvc
  Dream --> Summarizer
  Dream --> Adjudicator
  AgentLoop --> Reranker

  MemSvc --> Store
  Store --> Raft

  NodeSvc --> MemSvc
  NodeSvc --> Executor
  NodeSvc --> Policy
  NodeSvc --> Discovery
  NodeSvc --> MTLS

  Discovery -. peer list .-> NodeSvc
  MTLS -. creds .-> NodeSvc
  Raft -. consensus over mTLS gRPC .-> Raft
```

### Reading the diagram

- **Solid arrows** = runtime data/control flow.
- **Dashed arrows** = config read, reexec, inter-peer consensus — flows that don't fit the simple caller→callee model.
- **Boxes with a light-gray area** group components that ship together in a phase and share a natural API boundary.

### What's not shown

- **Per-request details** (request IDs, OpenTelemetry spans, logging) — cross-cutting, documented per subsystem.
- **Configuration flow** — `config.toml` is read once at boot and watched for reload; each component consumes its own section.
- **Skill + plugin system** (Phase 8) — will slot between the Channel layer and the Executor's Registry; not yet wired.

---

## Deployment topology

```mermaid
flowchart LR
  subgraph SingleNode["Single-node (personal use)"]
    N1["lobslaw<br/>all functions enabled"]
  end

  subgraph MultiNode["Multi-node cluster"]
    N2["node A<br/>memory + policy"]
    N3["node B<br/>memory + policy + gateway"]
    N4["node C<br/>compute + storage"]
    N2 <-- Raft/mTLS --> N3
    N3 <-- Raft/mTLS --> N4
    N2 <-- Raft/mTLS --> N4
  end
```

Any subset of functions (`memory`, `policy`, `compute`, `gateway`, `storage`) can run on each node. The Raft quorum is the subset of nodes running `memory` or `policy`.

---

## Phase status

| Phase | Components shipped | See |
|---|---|---|
| 1 | Foundation (config, logging, mTLS, crypto, types) | — |
| 2 | Cluster core (node.Node, Raft, discovery, gRPC) | [DISCOVERY.md](DISCOVERY.md) |
| 3 | Memory service (Store/Recall/Search/Forget, Dream, consolidation merge) | [MEMORY.md](MEMORY.md) |
| 4 | Tool execution (Registry, Policy, Hooks, Executor, Sandbox) | [SANDBOX.md](SANDBOX.md) |
| 5 | Agent Core + Provider Resolver + promptgen + LLM client + budget | [AGENT.md](AGENT.md) |
| 6 | REST + Telegram channels + confirmation prompts + JWT (HS256 + JWKS RS256/EdDSA) | [GATEWAY.md](GATEWAY.md) |
| 7 | Scheduler + PlanService + CAS-claim cluster coordination + built-in agent:turn handler | [SCHEDULER.md](SCHEDULER.md) |
| 8+ | Skills, Storage, SOUL, Audit, Polish | see PLAN.md |

---

## Inter-subsystem flows

The diagrams below are **retroactively added** per aide decision `lobslaw-documentation-diagrams` to bring shipped components into compliance. New flows land with their diagrams from day one.

### Tool invocation pipeline (Phase 4)

Agent loop → Executor → Registry → Policy → PreToolUse hook → Sandbox → subprocess → PostToolUse hook.

```mermaid
sequenceDiagram
  autonumber
  participant Agent as compute.Agent (Phase 5)
  participant Exec as compute.Executor
  participant Reg as compute.Registry
  participant Pol as policy.Engine
  participant Hook as hooks.Dispatcher
  participant SB as sandbox.Apply
  participant Tool as target subprocess

  Agent->>Exec: Invoke(InvokeRequest{tool, params, claims})
  Exec->>Reg: Get(tool)
  Reg-->>Exec: ToolDef
  Exec->>Exec: resolveToolPath (EvalSymlinks, root-contain)
  Exec->>Pol: Evaluate(claims, "tool:exec", tool)
  alt decision = deny / require_confirmation
    Pol-->>Exec: deny / require_confirmation
    Exec-->>Agent: error (ErrPolicyDenied / ErrRequireConfirm)
  else allow
    Pol-->>Exec: allow
    Exec->>Hook: PreToolUse(payload)
    alt hook blocks
      Hook-->>Exec: ErrHookBlocked
      Exec-->>Agent: error
    else hook allows
      Hook-->>Exec: ok
      Exec->>Exec: resolvePolicy(tool) → tool-spec → fleet → nil
      Exec->>SB: Apply(cmd, policy)
      Note over SB: may rewrite cmd<br/>to /proc/self/exe sandbox-exec<br/>if Policy has enforcement
      Exec->>Tool: exec (capped stdout/stderr, env whitelist)
      Tool-->>Exec: exit + output
      Exec->>Hook: PostToolUse(payload)
      Exec-->>Agent: InvokeResult
    end
  end
```

### Sandbox enforcement via reexec helper (Phase 4.5.5)

```mermaid
sequenceDiagram
  autonumber
  participant Parent as lobslaw (agent)
  participant Fork as fork'd child
  participant Helper as sandbox-exec<br/>(same binary, hidden subcmd)
  participant Kernel as Linux kernel
  participant Target as target tool

  Parent->>Parent: sandbox.Apply rewrites cmd<br/>cmd.Path = /proc/self/exe<br/>argv = ["lobslaw", "sandbox-exec", "--", "/real/tool", "args..."]<br/>env += LOBSLAW_SANDBOX_POLICY=<b64>
  Parent->>Fork: clone(Cloneflags: CLONE_NEWUSER|NEWNS|NEWPID|...)
  Note over Fork,Kernel: UID/GID mappings written<br/>by parent before child resumes
  Fork->>Helper: execve(/proc/self/exe sandbox-exec ...)
  Helper->>Helper: Decode policy from env,<br/>unset LOBSLAW_SANDBOX_POLICY
  Helper->>Kernel: prctl(PR_SET_NO_NEW_PRIVS, 1)
  Helper->>Kernel: landlock_create_ruleset + add_rule + restrict_self
  Helper->>Kernel: seccomp(SET_MODE_FILTER, TSYNC, &bpf)
  Helper->>Target: execve(/real/tool, args, env)
  Note over Target: Tool runs with all<br/>kernel enforcement active
```

See [SANDBOX.md](SANDBOX.md) for the library choices (go-landlock, elastic/go-seccomp-bpf), the rationale for the reexec pattern over alternatives, and the upstream Go proposal that may collapse this into stdlib.

### Memory dream cycle with merge phase (Phase 3.3 + 3.4)

```mermaid
sequenceDiagram
  autonumber
  participant Scheduler
  participant Dream as memory.DreamRunner
  participant Store as memory.Store
  participant Sum as Summarizer iface<br/>(Phase 5 LLM)
  participant Adj as Adjudicator iface<br/>(Phase 5 LLM; default stub)
  participant Raft

  Scheduler->>Dream: Run(ctx)
  Note over Dream: Skip if not Raft leader
  Dream->>Store: ForEach(episodic)
  Store-->>Dream: candidates
  Dream->>Dream: score (recency × importance)
  Dream->>Dream: selectTopN

  alt Summarizer wired (Phase 5+)
    Dream->>Sum: Summarize(candidates)
    Sum-->>Dream: summary + embedding
    Dream->>Raft: Apply(Put consolidated VectorRecord)
  end

  Dream->>Store: prune (score < threshold, non-long-term)
  Dream->>Raft: Apply(Delete low-score episodics)

  Note over Dream: Phase 2 — merge flow
  Dream->>Store: FindClusters(retention=long-term)
  Store-->>Dream: []Cluster

  loop each cluster
    Dream->>Adj: AdjudicateMerge(cluster)
    Adj-->>Dream: MergeDecision{verdict, text, reason}

    alt verdict = Merge
      Dream->>Raft: Apply(Put consolidated from MergedText)
      Dream->>Raft: Apply(Delete each source id)
    else verdict = Conflict
      Dream->>Raft: Apply(Put each record with metadata[conflict-cluster]=id)
    else verdict = Supersedes
      Dream->>Raft: Apply(Put each record with metadata[supersedes-chain]=id)
    else verdict = KeepDistinct (default / LLM error)
      Note over Dream: no action — conservative
    end
  end

  Dream->>Raft: Apply(Put dream-session episodic record)
  Dream-->>Scheduler: DreamResult{Consolidated, Pruned, Merge{...}}
```

### Forget cascade (Phase 3.2)

```mermaid
sequenceDiagram
  autonumber
  participant Caller
  participant Svc as memory.Service
  participant Scan as forgetScan
  participant Casc as forgetCascade
  participant Raft

  Caller->>Svc: Forget(ForgetRequest{query/tags/before/ids})
  Svc->>Svc: validate at-least-one-filter
  Svc->>Svc: check leader
  alt explicit ids provided
    Svc->>Svc: seed matched from ids
  end
  alt query/tags/before provided
    Svc->>Scan: scan(store, query, before, tags)
    Scan-->>Svc: matched source IDs
  end
  Svc->>Casc: cascade(store, matched)
  Note over Casc: any consolidated record whose SourceIDs<br/>intersect matched → included<br/>(aggressive sweep — see lobslaw-forget-cascade)
  Casc-->>Svc: swept IDs
  loop matched ∪ swept
    Svc->>Raft: Apply(Delete vector)
    Svc->>Raft: Apply(Delete episodic)
  end
  Svc-->>Caller: ForgetResponse{records_removed, consolidations_reforged}
```

### Policy evaluation (Phase 4.2)

```mermaid
sequenceDiagram
  autonumber
  participant Caller as Executor / RPC
  participant Eng as policy.Engine
  participant Store as memory.Store (rules)
  participant Eval as ConditionEvaluator

  Caller->>Eng: Evaluate(ctx, claims, action, resource)
  alt claims nil
    Eng-->>Caller: Decision{Deny, "no claims"}
  end
  Eng->>Store: load rules (descending priority)
  Store-->>Eng: rules
  loop each rule in priority order
    Eng->>Eng: subject match? action glob? resource glob? scope?
    alt skip
      Note over Eng: continue
    else match
      Eng->>Eval: conditionsHold(ctx, conditions) [RLock]
      alt evaluator errors
        Note over Eng: log warn, SKIP rule<br/>(⚠ see TODO: fail-closed)
      else conditions true
        Eng-->>Caller: Decision{rule.Effect, reason}
      end
    end
  end
  Note over Eng: no rule matched →<br/>Decision{Deny, "default deny"}
  Eng-->>Caller: Decision
```

### Channel request flow (Phase 6)

Inbound user message → channel → agent → back. Confirmations branch through the shared `PromptRegistry`.

```mermaid
sequenceDiagram
  autonumber
  participant Client
  participant CH as Channel<br/>(REST Server or<br/>TelegramHandler)
  participant Auth as pkg/auth.Validator<br/>(REST only)
  participant Agent as compute.Agent
  participant Reg as gateway.PromptRegistry

  Client->>CH: POST /v1/messages<br/>or Telegram webhook
  alt REST with validator
    CH->>Auth: Validate(bearer)
    Auth-->>CH: *types.Claims or error
  else Telegram
    CH->>CH: webhook-secret constant-time compare<br/>+ firstSeen(update_id) dedup<br/>+ resolveScope(user)
  end
  CH->>Agent: RunToolCallLoop(req)
  Agent-->>CH: ProcessMessageResponse

  alt resp.NeedsConfirmation
    CH->>Reg: Create(turnID, reason, channel, TTL)
    Reg-->>CH: Prompt{ID}
    alt REST
      CH-->>Client: 200 {prompt_id, needs_confirmation}
      Note over Client,Reg: client polls GET /v1/prompts/<id><br/>then POST /v1/prompts/<id>/resolve
    else Telegram
      CH->>Client: sendMessage + inline_keyboard<br/>callback_data "prompt:<verb>:<id>"
      Note over Client,Reg: user tap → callback_query webhook →<br/>handleCallbackQuery → Resolve
    end
  else plain reply
    CH-->>Client: 200 (JSON) or sendMessage (TG)
  end
```

See [GATEWAY.md](GATEWAY.md) for route tables, auth modes, dedup behaviour, and the PromptRegistry's atomic-resolve contract.

### Node startup and cluster join (Phase 2)

```mermaid
sequenceDiagram
  autonumber
  participant Op as Operator
  participant Node as node.Node
  participant MTLS as pkg/mtls
  participant Disc as discovery.Service
  participant Seed as Seed peer
  participant Raft

  Op->>Node: New(cfg) + Start(ctx)
  Node->>MTLS: LoadNodeCreds(ca, cert, key)
  MTLS-->>Node: NodeCreds
  Node->>Node: grpc.NewServer with mTLS
  Node->>Raft: NewRaft(transport using mTLS)
  Node->>Disc: start (seed + DNS + UDP broadcast)

  par Seed dial
    Disc->>Seed: dial over mTLS
    Seed-->>Disc: NodeInfo list
  and UDP broadcast (optional)
    Disc->>Disc: send announce
    Disc-->>Disc: receive announces from L2 peers
  end

  alt initial bootstrap
    Node->>Raft: Bootstrap(self-only)
  else join existing
    Node->>Seed: NodeService.AddMember(me)
    Seed->>Raft: ProposeConfChange(add voter)
    Raft-->>Seed: committed
    Seed-->>Node: ok; Raft streams replicate
  end

  Node-->>Op: Serving (SIGTERM → graceful Shutdown)
```

---

## Diagram maintenance

Per aide decision `lobslaw-documentation-diagrams`:

1. These diagrams MUST stay accurate. A PR that changes an underlying flow without updating its diagram is incomplete.
2. New flows ship with diagrams from day one.
3. Co-located with prose. Each subsystem doc owns its sequence diagrams; the architectural overview lives here.

The diagrams above are **retro-fitted** to bring Phases 1–4 into compliance. Any drift you spot → patch in the same commit as whatever caused it.
