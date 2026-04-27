---
topic: lobslaw-conversation-history
decision: "Telegram chatHistory now persists the FULL message thread per turn — user message + every intermediate assistant (with tool_calls) + every tool result + final assistant reply. Previously stored only [user, final_assistant_reply], stripping the bot's own actions; this caused the bot to deny tool usage on follow-up turns ('I don't have a web fetch tool') and confabulate fictional apologies. The 'maxTurns' field was always actually a message cap; renamed to maxMessages and bumped from 20 to 100 to accommodate multi-tool turns (each producing ~10 messages). newTurnMessages helper slices Agent's resp.Messages to skip system prompt + prior history, returning just the new turn's incremental messages. Mirrors opencode + Claude Code conversation primitives — assistant messages can carry tool_calls, tool messages carry results, all persist across turns."
date: 2026-04-24
---

# lobslaw-conversation-history

**Decision:** Telegram chatHistory now persists the FULL message thread per turn — user message + every intermediate assistant (with tool_calls) + every tool result + final assistant reply. Previously stored only [user, final_assistant_reply], stripping the bot's own actions; this caused the bot to deny tool usage on follow-up turns ('I don't have a web fetch tool') and confabulate fictional apologies. The 'maxTurns' field was always actually a message cap; renamed to maxMessages and bumped from 20 to 100 to accommodate multi-tool turns (each producing ~10 messages). newTurnMessages helper slices Agent's resp.Messages to skip system prompt + prior history, returning just the new turn's incremental messages. Mirrors opencode + Claude Code conversation primitives — assistant messages can carry tool_calls, tool messages carry results, all persist across turns.

## Rationale

Without persisting tool calls + results, the bot has no record of its own actions. When asked 'why did you look at openclaw?' it confabulates because it sees only [user, openclaw_answer] in history. Opencode's approach: one continuous conversation thread, never strip the middle. This is the canonical primitive in OpenAI/Anthropic schemas — assistant.tool_calls + role=tool messages stay in the array. Lobslaw was unique in stripping; the wedge bug was the cost.

