---
topic: lobslaw-tool-permissions
decision: "Tools not assumed to exist. Primary model is typed argv templates: ToolDef declares path + argv template with typed parameters, no shell by default. Policy gates all tool access with three-way effect (allow|deny|require_confirmation). Tool risk tier (reversible|communicating|irreversible) bridges to confirmation tier. Dangerous-filter deny-list is a last-resort safety net for operator-override tools only (where allowed_paths contains *), not the primary defence. Path enforcement via realpath + prefix check with O_NOFOLLOW. Sidecar model for elevated-access tools via narrow gRPC API"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-tool-permissions

**Decision:** Tools not assumed to exist. Primary model is typed argv templates: ToolDef declares path + argv template with typed parameters, no shell by default. Policy gates all tool access with three-way effect (allow|deny|require_confirmation). Tool risk tier (reversible|communicating|irreversible) bridges to confirmation tier. Dangerous-filter deny-list is a last-resort safety net for operator-override tools only (where allowed_paths contains *), not the primary defence. Path enforcement via realpath + prefix check with O_NOFOLLOW. Sidecar model for elevated-access tools via narrow gRPC API

## Rationale

Original dangerous-filter-primary model is a block-list and block-lists leak (lowercase rm, command substitution, find -delete). Argv-template primacy eliminates shell injection by construction. Adding risk tiers and require_confirmation gives a second defence when RBAC is insufficient (e.g. prompt injection persuading misuse of an allowed tool)

