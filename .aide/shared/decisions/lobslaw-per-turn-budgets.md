---
topic: lobslaw-per-turn-budgets
decision: "Per-turn/per-task budgets on: tool-call count, $ LLM spend, external-egress bytes. Soft defaults from config, overridable per task. Exceeding a budget raises a require_confirmation policy event via Channel.Prompt; continuing requires explicit user approval"
date: 2026-04-26
---

# lobslaw-per-turn-budgets

**Decision:** Per-turn/per-task budgets on: tool-call count, $ LLM spend, external-egress bytes. Soft defaults from config, overridable per task. Exceeding a budget raises a require_confirmation policy event via Channel.Prompt; continuing requires explicit user approval

## Rationale

Prompt-injection loops and runaway tool chains can burn wallet or spray external calls before anyone notices. Budgets are cheap, bounded, and failsafe. Treating exceedance as a confirmation event (rather than a hard stop) preserves user control without killing legitimate long-running work

