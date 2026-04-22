---
topic: lobslaw-hooks
decision: "Hook framework aligned with Claude Code event schema. Events: PreToolUse, PostToolUse, UserPromptSubmit, SessionStart, SessionEnd, Stop, Notification, PreCompact (Claude Code compat) plus PreLLMCall, PostLLMCall, PreMemoryWrite, PostMemoryRecall, ScheduledTaskFire, CommitmentDue (lobslaw-native). Hooks run as subprocess with JSON stdin/stdout; non-zero exit blocks"
date: 2026-04-22
---

# lobslaw-hooks

**Decision:** Hook framework aligned with Claude Code event schema. Events: PreToolUse, PostToolUse, UserPromptSubmit, SessionStart, SessionEnd, Stop, Notification, PreCompact (Claude Code compat) plus PreLLMCall, PostLLMCall, PreMemoryWrite, PostMemoryRecall, ScheduledTaskFire, CommitmentDue (lobslaw-native). Hooks run as subprocess with JSON stdin/stdout; non-zero exit blocks

## Rationale

Matching Claude Code hook schema lets the whole plugin ecosystem (RTK and others) drop in unchanged. Hooks are orthogonal to policy: policy says allow/deny/ask, hooks say transform before/after. Simple subprocess model - no in-process plugin runtime to maintain

