---
topic: commit-messages
decision: "Use Conventional Commits format with imperative mood. Never include AI-assistant co-author trailers (no 'Co-Authored-By: Claude' or similar). Commits should read as human-authored work product — the AI-assisted nature of development is a process detail, not something to attribute in the permanent commit history"
decided_by: johnm
date: 2026-04-22
---

# commit-messages

**Decision:** Use Conventional Commits format with imperative mood. Never include AI-assistant co-author trailers (no 'Co-Authored-By: Claude' or similar). Commits should read as human-authored work product — the AI-assisted nature of development is a process detail, not something to attribute in the permanent commit history

## Rationale

Prior guidance only covered Conventional Commits format and missed the attribution question. AI co-author trailers pollute git history with process metadata that has no durable value, complicates attribution/licensing downstream, and creates noise when reading git log. The value of AI assistance in development is real but it belongs in project docs or README, not in every commit trailer

