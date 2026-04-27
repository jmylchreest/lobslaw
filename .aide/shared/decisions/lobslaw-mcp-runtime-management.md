---
topic: lobslaw-mcp-runtime-management
decision: "mcp_list / mcp_add / mcp_remove builtins let the agent manage MCP integrations at runtime. MCPRegistry interface decouples compute (which defines the tools) from internal/mcp (which implements them) — loader satisfies the interface by name. Changes are node-local today (not Raft-replicated); operators wanting persistence edit [mcp.servers] in config. Builtins reject names containing '.', '/', '\\', or spaces because the name becomes the tool namespace prefix. Secrets should flow via secret_env config (future) rather than plaintext env through tool args; current schema accepts env only for non-sensitive values."
date: 2026-04-24
---

# lobslaw-mcp-runtime-management

**Decision:** mcp_list / mcp_add / mcp_remove builtins let the agent manage MCP integrations at runtime. MCPRegistry interface decouples compute (which defines the tools) from internal/mcp (which implements them) — loader satisfies the interface by name. Changes are node-local today (not Raft-replicated); operators wanting persistence edit [mcp.servers] in config. Builtins reject names containing '.', '/', '\\', or spaces because the name becomes the tool namespace prefix. Secrets should flow via secret_env config (future) rather than plaintext env through tool args; current schema accepts env only for non-sensitive values.

## Rationale

Mid-session addition of a new MCP server should not require a restart in single-node deployments. Raft-replicated config (so cluster members converge) is a follow-up because it needs a new proto message + LogEntry variant + FSM wiring — non-trivial scope that shouldn't block the single-node case where most personal deployments live. The name-constraint validation prevents breaking tool lookup — 'gmail.search' works only because 'gmail' has no dot.

