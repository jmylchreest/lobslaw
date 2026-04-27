---
topic: lobslaw-forced-summary-role
decision: "forceSummaryReply appends its closing instruction as role='user', not role='system'. MiniMax (and several other providers) reject mid-conversation system messages with HTTP 400 ('invalid message role: system'). The user-role nudge is universally accepted and operationally equivalent — the model treats it as a final-turn instruction. Without this fix every cap-hit produced the static fallback ('I hit my tool-call limit') even when the bot had real research data to synthesise from."
date: 2026-04-24
---

# lobslaw-forced-summary-role

**Decision:** forceSummaryReply appends its closing instruction as role='user', not role='system'. MiniMax (and several other providers) reject mid-conversation system messages with HTTP 400 ('invalid message role: system'). The user-role nudge is universally accepted and operationally equivalent — the model treats it as a final-turn instruction. Without this fix every cap-hit produced the static fallback ('I hit my tool-call limit') even when the bot had real research data to synthesise from.

## Rationale

Live test: bot completed 16 fetch_url calls navigating jmylchreest/lobslaw, captured useful state from README/DESIGN.md/fsm.go/buckets.go/agent.go/store.go, then hit MaxToolLoops. forceSummaryReply tried to inject a system message at conversation end → HTTP 400 from MiniMax → static fallback. User got 'I gave up' instead of a synthesis of the real findings. This is a wedge-class bug for any cap-hit turn.

