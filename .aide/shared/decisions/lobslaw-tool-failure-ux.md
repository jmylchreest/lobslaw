---
topic: lobslaw-tool-failure-ux
decision: "Fs/exec builtins return structured error JSON {error_type, message, suggestion} on failure; grep subprocess stderr captured and surfaced; agent loop injects a forced-summary assistant turn when max_tool_calls_per_turn is hit (no silent death); tool descriptions are explicit about local-vs-web scope so LLM doesn't reach for grep when user says 'on GitHub'."
date: 2026-04-24
---

# lobslaw-tool-failure-ux

**Decision:** Fs/exec builtins return structured error JSON {error_type, message, suggestion} on failure; grep subprocess stderr captured and surfaced; agent loop injects a forced-summary assistant turn when max_tool_calls_per_turn is hit (no silent death); tool descriptions are explicit about local-vs-web scope so LLM doesn't reach for grep when user says 'on GitHub'.

## Rationale

Observed a wedge where the LLM called search_files 6+ times in a row because rg returned opaque 'exit status 2' with no stderr, then silently died at the tool-call cap. Opencode returns structured errors with suggestions and this pattern prevents the doom-loop. Descriptions must be unambiguous about local-vs-remote scope because 'lobslaw code on GitHub' was interpreted as a filesystem search intent.

