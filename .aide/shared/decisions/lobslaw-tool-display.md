---
topic: lobslaw-tool-display
decision: "Tool-call breadcrumb removed. Telegram channel renders only the bot's reply, with no '_ran: X, Y, Z_' meta-prefix. Rationale: opencode/Claude-Code show tool calls inline because their UI is a developer tool — devs care about implementation. Telegram is a personal-assistant chat — the user wants the answer, not metadata. Even compact forms ('8× FetchUrl, 2× ReadFile') still cluttered the reply. Bot mentions tool actions in its own voice when relevant ('I checked the README and...'); transparency comes from the actual content, not from a meta-prefix. formatToolCall + prettyToolName helpers retained for policy-denial messages ('🚫 Policy denied X(args)') — that's user-actionable, not metadata."
date: 2026-04-24
---

# lobslaw-tool-display

**Decision:** Tool-call breadcrumb removed. Telegram channel renders only the bot's reply, with no '_ran: X, Y, Z_' meta-prefix. Rationale: opencode/Claude-Code show tool calls inline because their UI is a developer tool — devs care about implementation. Telegram is a personal-assistant chat — the user wants the answer, not metadata. Even compact forms ('8× FetchUrl, 2× ReadFile') still cluttered the reply. Bot mentions tool actions in its own voice when relevant ('I checked the README and...'); transparency comes from the actual content, not from a meta-prefix. formatToolCall + prettyToolName helpers retained for policy-denial messages ('🚫 Policy denied X(args)') — that's user-actionable, not metadata.

## Rationale

Operator looked at the live output: 16 FetchUrl(...) entries on one line was unreadable on a phone. Even after compacting to counts, the prefix added no value the bot's reply didn't already cover. Tool-call display is the wrong UX pattern for a chat interface; reserve the screen-real-estate for the answer.

