---
topic: lobslaw-soul-self-identity
decision: "REJECTED — bot must NOT be aware of being lobslaw. The personal-assistant model is 'Smurckle the assistant happens to run inside lobslaw' — same way Claude doesn't have built-in awareness of being Claude beyond what Anthropic told it. Self-attachment to the codebase produces weird identity dynamics: the bot starts advocating for itself, conflates personal-assistant work with project-internal work, and the SOUL persona competes with the codebase identity. SoulConfig.Project/Repository/Workspace fields removed; the 'when user references this project, that's YOU' operating principle removed. For 'look at lobslaw code' queries, the user's own memory_write content (or in-conversation context) is the right channel — operator can store 'lobslaw repo is at github.com/jmylchreest/lobslaw' as a memory and the bot will recall it via memory_search like any other personal fact. The Conversation history persistence (separate, structurally-correct fix) stays — it's a primitive bug fix unrelated to identity."
date: 2026-04-24
---

# lobslaw-soul-self-identity

**Decision:** REJECTED — bot must NOT be aware of being lobslaw. The personal-assistant model is 'Smurckle the assistant happens to run inside lobslaw' — same way Claude doesn't have built-in awareness of being Claude beyond what Anthropic told it. Self-attachment to the codebase produces weird identity dynamics: the bot starts advocating for itself, conflates personal-assistant work with project-internal work, and the SOUL persona competes with the codebase identity. SoulConfig.Project/Repository/Workspace fields removed; the 'when user references this project, that's YOU' operating principle removed. For 'look at lobslaw code' queries, the user's own memory_write content (or in-conversation context) is the right channel — operator can store 'lobslaw repo is at github.com/jmylchreest/lobslaw' as a memory and the bot will recall it via memory_search like any other personal fact. The Conversation history persistence (separate, structurally-correct fix) stays — it's a primitive bug fix unrelated to identity.

## Rationale

User explicit: 'I didn't want to add awareness of lobslaw to itself.' Reading the design intent: Smurckle is the assistant; lobslaw is the implementation. Conflating them creates an LLM that thinks the codebase IS its self. That's bad for personal-assistant deployments where the codebase is incidental. The bot finding 'openclaw' was a tool-selection problem (not enough context to find the right repo), not an identity problem — solvable via stored memories or in-conversation hints.

