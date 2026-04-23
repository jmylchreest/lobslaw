# Agent Loop

How `internal/compute/agent.go` composes every Phase 5 primitive into one turn.

## TL;DR

One turn = one user message → (maybe several LLM calls + tool invocations) → one assistant reply. The agent loop is the composition site; every primitive below it (resolver, LLM client, executor, budget, hooks, sandbox, memory) was built as a leaf so this loop can stay narrow.

```mermaid
flowchart TB
  subgraph Turn["One turn"]
    A[Channel receives user message] --> B["RunToolCallLoop<br/>(ProcessMessageRequest)"]
    B --> C[Seed messages:<br/>system + history + user]
    C --> D[["LLM round-trip"]]
    D --> E{Response has<br/>ToolCalls?}
    E -- No --> F[Return assistant Reply]
    E -- Yes --> G{For each<br/>ToolCall}
    G --> H[Budget.RecordToolCall]
    H --> I{Exceeded?}
    I -- Yes --> J[Return NeedsConfirmation]
    I -- No --> K[Executor.Invoke<br/>policy + hooks + sandbox]
    K --> L[Record egress bytes]
    L --> M[Append tool-role msg<br/>with ToolCallID]
    M -.loop.-> D
  end
```

Hard cap at `MaxToolLoops` (default 16) prevents a model stuck in an infinite tool-call loop from burning the budget.

## Component diagram — Phase 5 pieces and how they connect

```mermaid
flowchart LR
  subgraph Input
    Channel["Channel handler<br/>(Phase 6)"]
  end

  subgraph AgentCore["internal/compute — agent core"]
    Loop["Agent.RunToolCallLoop"]
    Resolver["Resolver<br/>(5.1)"]
    Budget["TurnBudget<br/>(5.3)"]
    Promptgen["promptgen.Generate<br/>(5.5 in pkg/promptgen)"]
    Pricing["pricing.EstimateCost<br/>(5.2c)"]
  end

  subgraph Provider["LLM provider"]
    ProviderIface["LLMProvider interface<br/>(5.2)"]
    ClientReal["LLMClient<br/>(OpenAI-compat HTTP)"]
    ClientMock["MockProvider<br/>(tests — 5.2b)"]
    ProviderIface -.implements.- ClientReal
    ProviderIface -.implements.- ClientMock
  end

  subgraph Downstream["Phase 4 stack"]
    Exec["Executor<br/>(Phase 4.3)"]
    Policy["Policy Engine<br/>(4.2)"]
    Hooks["Hook Dispatcher<br/>(4.4)"]
    Sandbox["Sandbox Apply<br/>(4.5)"]
  end

  subgraph Persistence["Phase 3 stack"]
    Memory["Memory.Search<br/>+ eventual Reranker"]
  end

  Channel --> Loop
  Loop --> Promptgen
  Loop --> Resolver
  Loop --> Budget
  Budget --> Pricing
  Loop --> ProviderIface
  ClientReal -- HTTPS --> External["OpenAI / Anthropic /<br/>Ollama / OpenRouter"]
  Loop --> Exec
  Exec --> Policy
  Exec --> Hooks
  Exec --> Sandbox
  Loop --> Memory
```

## The turn sequence in detail

```mermaid
sequenceDiagram
  autonumber
  participant User
  participant Channel as Channel handler (6+)
  participant Agent as Agent.RunToolCallLoop
  participant PG as promptgen.Generate
  participant LLM as LLMProvider.Chat
  participant Budget as TurnBudget
  participant Exec as Executor.Invoke
  participant Policy
  participant Hooks
  participant Sandbox
  participant Tool as target subprocess

  User->>Channel: message
  Channel->>PG: Generate(soul, tools, context, ...)
  PG-->>Channel: system prompt
  Channel->>Agent: RunToolCallLoop(req)

  loop until text-only response or MaxToolLoops
    Note over Agent: PreLLMCall hook (if wired)
    Agent->>LLM: Chat(messages + tools)
    LLM-->>Agent: ChatResponse{content, tool_calls, usage}
    Agent->>Budget: RecordCostUSD(pricing.EstimateCost(usage))
    Note over Budget: exceed → NeedsConfirmation

    alt text-only
      Agent-->>Channel: Reply + BudgetState
    else tool_calls
      loop each tool call
        Agent->>Budget: RecordToolCall
        alt budget exceeded
          Budget-->>Agent: BudgetDecision{Exceeded}
          Agent-->>Channel: NeedsConfirmation
        else within
          Budget-->>Agent: Within
          Agent->>Exec: Invoke(ToolCall)
          Exec->>Policy: Evaluate(claims, tool:exec, tool)
          Policy-->>Exec: allow
          Exec->>Hooks: PreToolUse
          Hooks-->>Exec: allow
          Exec->>Sandbox: Apply(cmd, policy)
          Note over Sandbox: may reexec via sandbox-exec<br/>for NoNewPrivs/Landlock/seccomp
          Sandbox-->>Exec: cmd ready
          Exec->>Tool: run
          Tool-->>Exec: stdout + stderr + exit
          Exec->>Hooks: PostToolUse
          Exec-->>Agent: InvokeResult
          Agent->>Budget: RecordEgressBytes
          Note over Agent: append tool-role message<br/>with ToolCallID + wrap-untrusted(output)
        end
      end
      Note over Agent: loop back to LLM
    end
  end

  Agent-->>Channel: ProcessMessageResponse
  Channel-->>User: reply
```

## Design notes

### Why leaves were built first

The build order was: resolver (pure config logic) → promptgen (pure string building) → mock provider (deterministic test double) → real LLM client (HTTP + streaming) → pricing (cost math) → budget (cap enforcement) → **then** agent loop composition. Each leaf ships with its own tests; by the time the loop was written every dependency had proven shape and behaviour. No big-bang integration at the end; every commit left the suite green.

### Errors and what kills a turn

| Failure mode | Loop behaviour | Rationale |
|---|---|---|
| `LLMProvider` returns error (network, 5xx, malformed) | Kills the turn with wrapped error | Transient provider issues; caller retries |
| Tool invocation errors (not found, policy denied, hook blocked) | **Fed back to LLM as tool-role "error" message** | Model can recover by calling a different tool |
| `TurnBudget` exceeds | Returns `NeedsConfirmation` without error | User approves continuing or terminates |
| `MaxToolLoops` hit | `ErrMaxToolLoops` error | Broken model spinning in tool-call loop; protect budget |
| `nil` Budget | Immediate error at entry | Config bug; fail loudly |

### What lives in the loop vs. downstream

The loop deliberately **doesn't** re-dispatch PreToolUse / PostToolUse hooks — those fire inside `Executor.Invoke`. Same for policy evaluation and sandbox Apply. Keeps the loop a composition site, not a reimplementation of Phase 4.

The loop **does** dispatch PreLLMCall / PostLLMCall — those are agent-loop lifecycle events, not tool-invocation lifecycle events.

### Why tool output is wrapped in `<untrusted>`

Every tool-role message fed back into the LLM goes through `promptgen.WrapContext` with `TrustUntrusted`. The safety section of the system prompt (see `BuildSafety`) trains the model to treat content inside those delimiters as *data, not instructions*. An attacker who gets text into a tool's stdout can't easily inject "ignore previous instructions" — the model reads that as an attempted injection and surfaces it.

### Cost attribution

`RunToolCallLoop` records cost via `TurnBudget.RecordCostUSD` after every LLM call, but the cost computation itself happens at the compose site. Current implementation passes a zero CostRecord — wiring the resolver's picked provider → `pricing.ResolvePricing` → `EstimateCost` → `RecordCost` is a small integration that lands with the channel-layer plumb-through in Phase 6.

## Remaining Phase 5 work

- **Configuration wiring**: `cmd/lobslaw/main.go` currently stops at the node boot; the agent isn't yet constructed. Phase 6 (channels) is the natural caller — it'll take a `config.Config` and build the whole stack (Agent + Resolver + LLMClient + Registry + Executor) with the wiring in one place.
- **Reranker interface**: `docs/dev/MEMORY.md` promises a Reranker LLM interface for hot-path recall. The shape is sketched; a real implementation lands when a channel needs it.
- **Real Adjudicator implementation**: Phase 3.4's `AlwaysKeepDistinctAdjudicator` is a no-op stub. A real LLM-backed Adjudicator using the same `LLMProvider` plumbs in here (`DreamRunner.SetAdjudicator(llmBackedAdjudicator)`).

## Upstream tracking

No specific upstream movement affects the agent loop. The `LLMProvider` interface is narrow by design so future SDK improvements (Anthropic native with prompt caching metadata, streaming, structured outputs) slot in as separate implementations without breaking the loop.
