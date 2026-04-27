---
topic: lobslaw-self-introspection
decision: "debug_tools, debug_memory, debug_storage, debug_providers, memory_recent now permanent in toolTailorDefaults — every turn admits these regardless of intent. Without them, the bot saw a contextually-tailored function-calling schema (3-4 tools), reported THAT as its full capability ('memory is empty, my tools are X, Y, Z'), and could neither verify state nor enumerate registered tools. Self-introspection is now structurally available on every turn. Operating Principles addendum: 'Never assert X is empty without checking — call debug_* / memory_recent first.' Tool tailor safety-net rewritten to bare-zero: only fall back to advertise-everything when the heuristic returns 0 tools, not when it returns fewer than len(defaults) (the prior shape over-fired whenever the registry was a strict subset of defaults)."
date: 2026-04-25
---

# lobslaw-self-introspection

**Decision:** debug_tools, debug_memory, debug_storage, debug_providers, memory_recent now permanent in toolTailorDefaults — every turn admits these regardless of intent. Without them, the bot saw a contextually-tailored function-calling schema (3-4 tools), reported THAT as its full capability ('memory is empty, my tools are X, Y, Z'), and could neither verify state nor enumerate registered tools. Self-introspection is now structurally available on every turn. Operating Principles addendum: 'Never assert X is empty without checking — call debug_* / memory_recent first.' Tool tailor safety-net rewritten to bare-zero: only fall back to advertise-everything when the heuristic returns 0 tools, not when it returns fewer than len(defaults) (the prior shape over-fired whenever the registry was a strict subset of defaults).

## Rationale

Live test: bot was asked 'anything else?' after a restart. Reported 'Memory is empty' (state.db is 2.0MB with 36+ episodic records) and listed 4 of 30 tools as its capability set. Cause: tailor admitted only 3-4 tools for the vague follow-up, bot reported what it saw in its schema, never called any debug tool to verify. The bot had no path to ground-truth its own state because the introspection tools weren't in the schema. Making them defaults is the cheap structural fix; the operating principle is the prompt-side reinforcement.

