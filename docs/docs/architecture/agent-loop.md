---
sidebar_position: 4
---

# Agent Loop

How a single turn runs from message arrival to reply.

```
┌──────────────────────────────────────────────────────────────┐
│  1. Channel receive                                          │
│     gateway/telegram, gateway/rest, gateway/webhook          │
│     ─► turn = {user_id, channel, channel_id, text, attachments}│
└──────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌──────────────────────────────────────────────────────────────┐
│  2. Auth + claims resolution                                 │
│     - Gateway-specific (Telegram chat_id lookup,             │
│       JWT validation, webhook HMAC)                          │
│     - Output: Claims{scope, user_id, subject}                │
└──────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌──────────────────────────────────────────────────────────────┐
│  3. Context assembly (compute/context_engine)                │
│     - Recent episodic (last N turns)                         │
│     - Top-k semantic recall on user message                  │
│     - Active soul fragments                                  │
│     - Recent dream                                           │
│     - Active commitments + scheduled tasks                   │
│     - User timezone, channel context                         │
│     ─► system prompt + history                               │
└──────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌──────────────────────────────────────────────────────────────┐
│  4. LLM call (compute/agent.Run)                             │
│     - tools = registry.LLMTools()  (no tailoring; full list) │
│     - provider = roles.Worker or default                     │
│     - response: text + tool_calls                            │
└──────────────────────────────────────────────────────────────┘
                            │
                ┌───────────┼───────────┐
                ▼                       ▼
┌─────────────────────┐     ┌─────────────────────────────────┐
│  No tool calls      │     │  Tool calls                      │
│  ─► reply to user   │     │  for each:                       │
│                     │     │    policy.Evaluate               │
│                     │     │    PreToolUse hooks              │
│                     │     │    dispatch:                     │
│                     │     │      - builtin (in-proc)         │
│                     │     │      - skill (sandbox subprocess)│
│                     │     │      - MCP (subprocess)          │
│                     │     │    PostToolUse hooks             │
│                     │     │    write episodic record         │
│                     │     │  ─► loop back to step 4 with     │
│                     │     │      tool results in context     │
└─────────────────────┘     └─────────────────────────────────┘
```

## Tool dispatch

`compute.Executor.Call(ctx, claims, tool, args)`:

1. **Policy gate.** `policy.Evaluate(claims, "tool:exec", tool)` — allow / deny / require_confirmation. If require_confirmation, pause turn and ask the channel; resume when user replies.
2. **PreToolUse hook.** `hooks.Dispatch("PreToolUse", {tool, args, claims})` — arbitrary scripts can block.
3. **Resolve tool kind.** Builtin (path starts with `BuiltinScheme://`), skill (path is `skill://<name>/<tool>`), or MCP (path is `mcp://<server>/<tool>`).
4. **Inject synthetic args.** Some args the agent doesn't see in the schema but get added at dispatch time: `__channel`, `__chat_id`, `__user_id`, `__user_timezone`. Used by `notify` for routing, by `commitment_create` for parse, etc.
5. **Dispatch.** Builtin runs in-process; skill spawns a sandboxed subprocess; MCP routes to the long-lived MCP server's stdio.
6. **PostToolUse hook.** `hooks.Dispatch("PostToolUse", {tool, args, result})`.
7. **Episodic record.** Write `EpisodicRecord{event="tool_call_done", context=...}` so the next turn can recall.

## Loops

The agent loops while the LLM returns tool calls. The loop has a `max_tool_calls_per_turn` cap (default 25) and a `max_turn_seconds` cap (default 600). Hitting either kills the turn and returns whatever's been said so far.

## Multimodality

Attachments downloaded to `/workspace/incoming/<turn_id>/`. The user message is decorated:

```
[user attached: /workspace/incoming/01HX.../image_001.png]
What's in this image?
```

The agent sees the local path and chooses to call `read_image(path="...")`, `read_audio(path="...")`, or `read_pdf(path="...")` based on the file. These builtins route to providers with the right capability (see [Providers](/configuration/providers)).

## Why the agent loop bypasses memory recall for some flows

Research planner + synth go through `LLMProvider.Chat` directly, not through the agent loop. Reason: episodic recall would inject prior research reports into the planner's context, breaking JSON parse.

Worker turns *do* go through the agent loop (they need tool access). The bypass is only for the planner + synth.

See [Research](/features/research) for details.

## Reference

- `internal/compute/agent.go` — Run loop
- `internal/compute/executor.go` — Call (policy gate + dispatch)
- `internal/compute/context_engine.go` — Assemble
- `internal/compute/registry.go` — LLMTools, RegisterExternal
- `pkg/promptgen/sections.go` — Operating Principles
