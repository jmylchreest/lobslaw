---
topic: lobslaw-binary-model
decision: "Single binary combines all node functions (memory, storage, policy, compute, gateway). Execute with enabled functions via flags/config. Single-agent mode = all functions. Dedicated nodes = subset"
date: 2026-04-22
---

# lobslaw-binary-model

**Decision:** Single binary combines all node functions (memory, storage, policy, compute, gateway). Execute with enabled functions via flags/config. Single-agent mode = all functions. Dedicated nodes = subset

## Rationale

Operationally simple: one binary, different configurations. Single-agent most likely; scale by splitting functions across nodes

