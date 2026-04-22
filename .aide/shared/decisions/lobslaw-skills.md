---
topic: lobslaw-skills
decision: "Skills registry with clawhub.ai-compatible manifest (OpenClaw manifest format). Skills live in storage as directories with manifest.yaml + handler + optional SKILL.md/SOUL.md. Trust model per lobslaw-skill-trust (trust-on-install-after-operator-review, optional minisign). Invocation format compatible with OpenClaw so clawhub sync works unchanged. Skills can be shipped inside lobslaw plugin directories (see lobslaw-plugins) alongside hooks and MCP server declarations"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-skills

**Decision:** Skills registry with clawhub.ai-compatible manifest (OpenClaw manifest format). Skills live in storage as directories with manifest.yaml + handler + optional SKILL.md/SOUL.md. Trust model per lobslaw-skill-trust (trust-on-install-after-operator-review, optional minisign). Invocation format compatible with OpenClaw so clawhub sync works unchanged. Skills can be shipped inside lobslaw plugin directories (see lobslaw-plugins) alongside hooks and MCP server declarations

## Rationale

Original decision named the registry feature set. Expanded to: trust model explicit (see lobslaw-skill-trust), plugin directory format explicit (see lobslaw-plugins), and OpenClaw compatibility preserved. Skills are now one of several artefacts a plugin can ship, not the only one

