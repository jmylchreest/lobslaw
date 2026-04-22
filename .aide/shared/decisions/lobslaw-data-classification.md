---
topic: lobslaw-data-classification
decision: "Per-provider trust_tier (local|private|public). Per-scope and per-chain min_trust_tier floor. Resolver refuses to route a turn through any provider below the required tier; on failure the turn either fails closed or surfaces a user-visible notice with fallback option"
date: 2026-04-22
---

# lobslaw-data-classification

**Decision:** Per-provider trust_tier (local|private|public). Per-scope and per-chain min_trust_tier floor. Resolver refuses to route a turn through any provider below the required tier; on failure the turn either fails closed or surfaces a user-visible notice with fallback option

## Rationale

Every LLM turn sends the conversation context - memories, documents, private data - to the provider. Classification is the single biggest privacy lever. Cheap to add now (~30 lines design, one field per provider, one on chain/scope, one resolver check); much harder to retrofit once memory has already touched mixed-trust providers

