---
topic: lobslaw-confirmation-tier
decision: "Three-way policy effect: allow|deny|require_confirmation. Confirmation is delivered inline on the originating channel via Channel.Prompt(user, question, options, timeout) primitive. Risk tier on ToolDef: reversible|communicating|irreversible. Policy rules can require confirmation based on tier, argv patterns, or arbitrary conditions. On timeout the default outcome is deny"
date: 2026-04-22
---

# lobslaw-confirmation-tier

**Decision:** Three-way policy effect: allow|deny|require_confirmation. Confirmation is delivered inline on the originating channel via Channel.Prompt(user, question, options, timeout) primitive. Risk tier on ToolDef: reversible|communicating|irreversible. Policy rules can require confirmation based on tier, argv patterns, or arbitrary conditions. On timeout the default outcome is deny

## Rationale

Binary allow/deny is too coarse for a personal assistant that can do anything. Confirmation tier is the second defence when RBAC alone isn't enough (e.g. prompt injection persuading the agent to misuse an allowed tool). Inline on original channel keeps UX simple - no separate approvals surface

