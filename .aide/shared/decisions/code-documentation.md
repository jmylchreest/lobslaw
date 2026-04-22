---
topic: code-documentation
decision: "Code should be self-documenting; write docs for WHY, not WHAT"
decided_by: blueprint:general@0.0.61
date: 2026-04-21
---

# code-documentation

**Decision:** Code should be self-documenting; write docs for WHY, not WHAT

## Rationale

Comments that restate what code does become stale and misleading. Documentation that explains intent, constraints, and non-obvious decisions remains valuable as code evolves.

## Details

Name things clearly. Document: public API contracts, non-obvious design decisions, performance constraints, security boundaries. Keep docs next to code (doc comments, package-level README). Use ADRs for cross-cutting architectural decisions.

