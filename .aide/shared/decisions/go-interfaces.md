---
topic: go-interfaces
decision: "Define interfaces at the consumer, keep them small (1-3 methods), accept interfaces and return concrete types"
decided_by: blueprint:go@0.0.59
date: 2026-04-21
references:
  - https://go.dev/doc/effective_go#interfaces
---

# go-interfaces

**Decision:** Define interfaces at the consumer, keep them small (1-3 methods), accept interfaces and return concrete types

## Rationale

Consumer-side interfaces decouple packages without premature abstraction. Small interfaces are easier to implement, compose, and mock. Returning concrete types preserves full API surface for callers who do not need abstraction.

## Details

- Define at consumer, not producer. 1-3 methods max.
- Compose larger behaviours from small interfaces (io.ReadWriteCloser).
- No interface until two implementations or test mocking requires it.
- Accept interfaces, return concrete types.

