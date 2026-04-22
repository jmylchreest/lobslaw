---
topic: go-concurrency
decision: "Use errgroup over WaitGroup; bound all goroutine counts; channels for ownership transfer, mutexes for shared state"
decided_by: blueprint:go@0.0.59
date: 2026-04-21
references:
  - https://pkg.go.dev/golang.org/x/sync/errgroup
---

# go-concurrency

**Decision:** Use errgroup over WaitGroup; bound all goroutine counts; channels for ownership transfer, mutexes for shared state

## Rationale

errgroup handles error propagation and context cancellation that WaitGroup ignores. Unbounded goroutines cause resource exhaustion under load. Channels and mutexes solve different problems — mixing them causes deadlocks.

## Details

- errgroup over WaitGroup. Always g.SetLimit(n) to bound goroutine count.
- Channels for ownership transfer; mutexes for shared state. Don't mix for same resource.
- All loops: check ctx.Err() or select on ctx.Done().
- Capture loop vars explicitly in goroutine closures for clarity.

