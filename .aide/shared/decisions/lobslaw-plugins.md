---
topic: lobslaw-plugins
decision: "Plugin directory format compatible with Claude Code: plugin.toml (or plugin.json) + optional hooks/, skills/, commands/, agents/, .mcp.json. lobslaw plugin install|enable|import supports git URL, local path, clawhub ref, and Claude Code plugin directories. Importing a Claude Code plugin is a mechanical copy since hook schema matches. MCP servers declared in .mcp.json are consumed as tools through the same tool registry with policy gating"
date: 2026-04-22
---

# lobslaw-plugins

**Decision:** Plugin directory format compatible with Claude Code: plugin.toml (or plugin.json) + optional hooks/, skills/, commands/, agents/, .mcp.json. lobslaw plugin install|enable|import supports git URL, local path, clawhub ref, and Claude Code plugin directories. Importing a Claude Code plugin is a mechanical copy since hook schema matches. MCP servers declared in .mcp.json are consumed as tools through the same tool registry with policy gating

## Rationale

Matching Claude Code plugin format unlocks the existing ecosystem (RTK, playwright-mcp, chrome-devtools-mcp, etc) with zero bespoke integration. clawhub compatibility for the skill half; Claude Code compat for the hook+MCP half; they don't conflict

