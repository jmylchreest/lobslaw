---
sidebar_position: 5
---

# Research

Deep-research framework: planner → workers → synth.

When a user asks a research question that's bigger than a single turn ("what's the state of X technology?", "compare these 5 providers"), the agent fans out:

```
              user question
                   │
                   ▼
              ┌───────────┐
              │  planner  │   produce N independent sub-questions
              └─────┬─────┘
                    │  fan out
        ┌───────────┼───────────┐
        ▼           ▼           ▼
   ┌──────┐    ┌──────┐    ┌──────┐
   │ wkr1 │    │ wkr2 │    │ wkr3 │   each runs a full agent turn
   └──┬───┘    └──┬───┘    └──┬───┘   with web_search + read_pdf + ...
      │           │           │
      └───────────┼───────────┘
                  │  fan in
                  ▼
              ┌───────────┐
              │   synth   │   merge findings, narrate
              └─────┬─────┘
                    │
                    ▼
              user (notify)
```

## When it's used

Triggered by the `research_start` builtin:

> **You:** research what's currently the best multimodal LLM for low-latency vision; compare Claude, GPT, Gemini, MiniMax, and any open-source models people are talking about.
>
> **Bot:** Started research task `01HX...`. I'll ping you with findings — usually 2-5 minutes.
>
> *(later)*
>
> **Bot:** **Multimodal LLM comparison (research:01HX...)**
>
> **TL;DR:** For low-latency vision, ...
>
> **Findings:**
>
> *Claude Sonnet 4 — ...*
> *GPT-4.1 — ...*
> *...*

## Why fan-out?

A single agent turn is rate-limited (provider RPM, context window). Five parallel turns hit the rate limit five times in parallel and finish ~5× faster. The synth turn aggregates without paying the per-question round-trip.

It also helps with quality: workers see only their sub-question, so they don't conflate sources. The synth has all worker outputs but is asked to compare and structure rather than re-fetch.

## Planner and synth bypass the agent loop

The planner and synth are pure prompt → JSON / prompt → markdown turns — they don't need tool access. They go through `compute.LLMProvider.Chat` directly, **not** through the agent loop.

Why? Because the agent loop loads episodic memory by default. If the user did a similar research task last week, recall would inject *that* report into the planner's context, and the JSON parse would fail (the prior report is markdown). Bypassing the agent loop keeps the planner's context tight: just `[system, user]`, no episodic spillover.

Workers, by contrast, *do* go through the agent loop — they need web search, read_url, read_pdf, etc.

## Robustness

- **Planner JSON parse failure** → retry once with a tightened prompt (fewer-tokens + explicit "no markdown fences" instruction).
- **Worker timeout** → drop that worker's results; synth runs with what came back.
- **Synth empty output** → return error to the originating commitment; user gets a "research failed, retry?" message.

## Notification path

Research is asynchronous — user-initiated, fires later. The pattern:

1. User invokes `research_start(question="...")`.
2. Builtin opens an `AgentCommitment` with `prompt=research:run` and immediately returns "started".
3. Scheduler fires the commitment a few seconds later.
4. The commitment's runner spawns a `research.Coordinator`, which runs planner → workers → synth.
5. Synth output is `notify`-ed back to the user via the originating channel.

Same firing path as commitments, same `notify` sink layer — research is just a commitment with a built-in pipeline.

## Auditing what happened

Every worker output and the final synth are written to episodic memory:

- `Event` field: "research_worker_done" / "research_synth_done"
- `Context` field: question + worker output
- `subject` claim attached for retrieval

Search later via `memory_recall(query="multimodal LLM comparison")`.

## Tuning

```toml
[compute.research]
worker_count       = 5         # default; planner can request fewer
worker_timeout     = "2m"
synth_timeout      = "1m"
worker_role        = "worker"   # which provider role workers use
synth_role         = "default"  # synth uses primary provider
```

## Reference

- `internal/compute/research/research.go` — Coordinator (plan, workers, synth)
- `internal/compute/builtin_research.go` — agent-facing `research_start`, `research_cancel`
- `internal/node/wire_compute.go` — `runResearchCommitment` glue
- `internal/notify/` — synth output dispatch
