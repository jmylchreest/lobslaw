---
topic: lobslaw-tool-loop-cap
decision: "DefaultMaxToolLoops bumped from 16 to 24. Empirical: thorough single-repo research (fetch landing page → walk a few directories → read a few files → synthesise) lands around 16-20 calls. 16 was hitting mid-research with no margin to wrap up. 24 keeps the broken-loop pathology bounded (a stuck model still hits the wall before racking up real money) while letting genuine multi-step research turns complete."
date: 2026-04-24
---

# lobslaw-tool-loop-cap

**Decision:** DefaultMaxToolLoops bumped from 16 to 24. Empirical: thorough single-repo research (fetch landing page → walk a few directories → read a few files → synthesise) lands around 16-20 calls. 16 was hitting mid-research with no margin to wrap up. 24 keeps the broken-loop pathology bounded (a stuck model still hits the wall before racking up real money) while letting genuine multi-step research turns complete.

## Rationale

Live test produced 16 productive fetch_url calls and ran out of room. Bumping is cheaper than the alternative: every cap-hit forces a forceSummaryReply round-trip anyway, so the floor on tokens-per-turn doesn't change much. 24 is still cap-bounded — model can't run away forever.

