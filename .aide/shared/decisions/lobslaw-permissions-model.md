---
topic: lobslaw-permissions-model
decision: "Policy sync'd to all nodes. PolicyNode is system of record. Enforcement at execution point: wire protocol server-side AND agent local tool execution. Not duplicated - enforced where relevant"
date: 2026-04-22
---

# lobslaw-permissions-model

**Decision:** Policy sync'd to all nodes. PolicyNode is system of record. Enforcement at execution point: wire protocol server-side AND agent local tool execution. Not duplicated - enforced where relevant

## Rationale

Each node enforces locally but policy comes from central source. Avoids duplication by having clear enforcement boundaries

