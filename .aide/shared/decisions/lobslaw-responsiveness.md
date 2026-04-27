---
topic: lobslaw-responsiveness
decision: "Gateway turn responsiveness has three configurable timers under [gateway]: typing_interval (default 4s, refreshes Telegram typing indicator), interim_timeout (default 30s, sends 'still working' message only when SOUL characteristic style=chatty), hard_timeout (default 90s, cancels turn context and forces a final-summary assistant turn with tools disabled). SOUL frontmatter YAML block exposes structured characteristics {style: direct|chatty, certainty: low|medium|high}; parsed at load, surfaced via Soul.Characteristics() alongside Text()."
date: 2026-04-24
---

# lobslaw-responsiveness

**Decision:** Gateway turn responsiveness has three configurable timers under [gateway]: typing_interval (default 4s, refreshes Telegram typing indicator), interim_timeout (default 30s, sends 'still working' message only when SOUL characteristic style=chatty), hard_timeout (default 90s, cancels turn context and forces a final-summary assistant turn with tools disabled). SOUL frontmatter YAML block exposes structured characteristics {style: direct|chatty, certainty: low|medium|high}; parsed at load, surfaced via Soul.Characteristics() alongside Text().

## Rationale

User hit a wedge where the bot silently looped tools for 10+ calls and never replied — no typing indicator, no progress message, no forced summary. Typing indicator is universal (always on — cheap), interim messages are personality-gated (direct SOUL bot shouldn't chatter filler; chatty SOUL bot should), hard timeout is universal safety valve. Zero disables a timer. Frontmatter chosen over separate config so personality stays coherent in one file: LLM-facing text + runtime behaviour together; operator edits SOUL.md once, hot-reload picks up both.

