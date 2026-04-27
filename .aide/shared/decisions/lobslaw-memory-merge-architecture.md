---
topic: lobslaw-memory-merge-architecture
decision: "Memory service provides deterministic primitives (Search cosine, FindClusters cosine+union-find, Forget with ids). LLM layer provides interpretation via three SEPARATE interfaces: Summarizer (episodic → consolidated narrative), Adjudicator (cluster → merge verdict), Reranker (candidates → LLM-filtered top-N). Caller orchestrates composition (Search→Rerank for hot-path recall; FindClusters→Adjudicate for Dream merge; Search→Forget by ids for user-initiated topic forget)."
date: 2026-04-23
---

# lobslaw-memory-merge-architecture

**Decision:** Memory service provides deterministic primitives (Search cosine, FindClusters cosine+union-find, Forget with ids). LLM layer provides interpretation via three SEPARATE interfaces: Summarizer (episodic → consolidated narrative), Adjudicator (cluster → merge verdict), Reranker (candidates → LLM-filtered top-N). Caller orchestrates composition (Search→Rerank for hot-path recall; FindClusters→Adjudicate for Dream merge; Search→Forget by ids for user-initiated topic forget).

