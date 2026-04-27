---
topic: lobslaw-path-discovery
decision: "Boot-time enumerate $PATH for executable regular files; subtract a hardcoded common-unix baseline (git, grep, find, curl, jq, sed, awk, tar, gzip, xargs, etc. ~60 entries); inject the remainder plus 'typical unix commands are also available' line into the system prompt via promptgen. Cached for the process lifetime; refreshed on node restart."
date: 2026-04-24
---

# lobslaw-path-discovery

**Decision:** Boot-time enumerate $PATH for executable regular files; subtract a hardcoded common-unix baseline (git, grep, find, curl, jq, sed, awk, tar, gzip, xargs, etc. ~60 entries); inject the remainder plus 'typical unix commands are also available' line into the system prompt via promptgen. Cached for the process lifetime; refreshed on node restart.

## Rationale

Bot was guessing filesystem paths and didn't know what binaries were available. Full $PATH dump would be noisy; training-distribution commands don't need advertising; specialty binaries (rtk, bunx, himalaya) genuinely do. Boot-time snapshot is zero-per-turn overhead. Opencode uses a similar pattern for env context but doesn't enumerate — we enumerate for the specialty subset because our tool container will increasingly diverge from a standard Unix baseline.

