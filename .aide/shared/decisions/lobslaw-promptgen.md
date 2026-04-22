---
topic: lobslaw-promptgen
decision: "promptgen package: GeneratorInput (soul/tools/skills/mode/env) → Prompt (system + context blocks). Section builders for each prompt part. Bootstrap loader with per-file and total truncation. Clean separation from agent loop, independently testable"
date: 2026-04-22
---

# lobslaw-promptgen

**Decision:** promptgen package: GeneratorInput (soul/tools/skills/mode/env) → Prompt (system + context blocks). Section builders for each prompt part. Bootstrap loader with per-file and total truncation. Clean separation from agent loop, independently testable

## Rationale

System prompt assembly is a distinct concern. Separating it makes prompts testable, allows different prompt modes without agent loop changes, and isolates the trimming logic

