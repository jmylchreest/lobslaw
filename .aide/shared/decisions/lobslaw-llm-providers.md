---
topic: lobslaw-llm-providers
decision: "OpenAI API-compatible multi-provider: OpenRouter, MiniMax, Anthropic, etc via OpenAI-compatible API. Multiple endpoints with labels for task routing, complexity selection, and failover"
date: 2026-04-22
---

# lobslaw-llm-providers

**Decision:** OpenAI API-compatible multi-provider: OpenRouter, MiniMax, Anthropic, etc via OpenAI-compatible API. Multiple endpoints with labels for task routing, complexity selection, and failover

## Rationale

OpenAI API is the de-facto standard; multi-provider with labels allows logical routing by task/complexity and automatic failover

