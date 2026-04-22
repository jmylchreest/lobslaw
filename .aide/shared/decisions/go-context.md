---
topic: go-context
decision: context.Context is always the first parameter named ctx; use typed keys; always defer cancel
decided_by: blueprint:go@0.0.59
date: 2026-04-21
references:
  - https://go.dev/blog/context
---

# go-context

**Decision:** context.Context is always the first parameter named ctx; use typed keys; always defer cancel

## Rationale

Consistent placement makes context visible in every function signature. Typed keys prevent collisions across packages. Deferred cancel prevents context leaks which leak goroutines and memory.

## Details

- First param, named ctx. Never store in a struct.
- WithValue keys: typed unexported struct pointers, never strings.
- Always defer cancel() immediately after WithTimeout/WithCancel.
- context.WithoutCancel(ctx) for fire-and-forget goroutines — preserves values without inheriting cancellation.

