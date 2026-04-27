---
topic: lobslaw-proactive-messaging
decision: "Three new builtins (commitment_create / commitment_list / commitment_cancel + notify_telegram) plus channel-context propagation give the bot end-to-end proactive messaging. Architecture mirrors the existing primitive split: ScheduledTaskRecord (cron, recurring) for 'every 5m check mail'; AgentCommitment (one-shot, due_at) for 'in 2 minutes message me'. Both fire through the same scheduler tick loop, both dispatch via runCommitmentAsAgentTurn / runTaskAsAgentTurn handlers that already existed. The missing wires were (a) agent-callable CRUD on AgentCommitment, (b) a proactive-push tool, (c) chat_id propagation. notify_telegram takes chat_id+text, calls TelegramHandler.Send (new public method). RuntimeInfo gains Channel + ChannelID fields rendered in the CONTEXT-tier Runtime section so the bot sees its origin chat each turn and can embed it into commitment prompts. parseWhen accepts Go durations ('2m', '1h') OR RFC3339 timestamps."
date: 2026-04-25
---

# lobslaw-proactive-messaging

**Decision:** Three new builtins (commitment_create / commitment_list / commitment_cancel + notify_telegram) plus channel-context propagation give the bot end-to-end proactive messaging. Architecture mirrors the existing primitive split: ScheduledTaskRecord (cron, recurring) for 'every 5m check mail'; AgentCommitment (one-shot, due_at) for 'in 2 minutes message me'. Both fire through the same scheduler tick loop, both dispatch via runCommitmentAsAgentTurn / runTaskAsAgentTurn handlers that already existed. The missing wires were (a) agent-callable CRUD on AgentCommitment, (b) a proactive-push tool, (c) chat_id propagation. notify_telegram takes chat_id+text, calls TelegramHandler.Send (new public method). RuntimeInfo gains Channel + ChannelID fields rendered in the CONTEXT-tier Runtime section so the bot sees its origin chat each turn and can embed it into commitment prompts. parseWhen accepts Go durations ('2m', '1h') OR RFC3339 timestamps.

## Rationale

User asked 'in 2 minutes message me to tell me I am great'. Bot correctly identified the gap (no proactive push) but also lied about not having a scheduler tool. Real fix: not a prompt change but a missing capability. AgentCommitment is the right primitive — it's already in the proto + scheduler. User's design instinct was correct: cron-style ticker checks one-shot table; that's exactly how the existing scheduler treats commitments. Just needed the LLM-accessible surface and the push wire.

