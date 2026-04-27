---
topic: lobslaw-documentation-diagrams
decision: "All significant agent flows, processes, and components MUST be documented with mermaid diagrams. Minimum coverage: (1) one overarching C4-style component diagram showing how services/packages compose; (2) one sequence/flow diagram per significant process (agent loop, tool invocation, memory dream cycle, sandbox-exec reexec, policy evaluation, mTLS handshake + node join, forget cascade, memory merge pipeline). Diagrams live alongside the relevant prose in docs/*.md — not in a separate diagrams/ dir. Update rule: when the underlying flow changes, the diagram MUST be updated in the same commit. PRs that change a documented flow but don't update its diagram are incomplete."
date: 2026-04-23
---

# lobslaw-documentation-diagrams

**Decision:** All significant agent flows, processes, and components MUST be documented with mermaid diagrams. Minimum coverage: (1) one overarching C4-style component diagram showing how services/packages compose; (2) one sequence/flow diagram per significant process (agent loop, tool invocation, memory dream cycle, sandbox-exec reexec, policy evaluation, mTLS handshake + node join, forget cascade, memory merge pipeline). Diagrams live alongside the relevant prose in docs/*.md — not in a separate diagrams/ dir. Update rule: when the underlying flow changes, the diagram MUST be updated in the same commit. PRs that change a documented flow but don't update its diagram are incomplete.

