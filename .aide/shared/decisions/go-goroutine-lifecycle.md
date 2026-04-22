---
topic: go-goroutine-lifecycle
decision: Every goroutine must have a guaranteed exit path; no unbounded loops without cancellation
decided_by: blueprint:go@0.0.59
date: 2026-04-21
---

# go-goroutine-lifecycle

**Decision:** Every goroutine must have a guaranteed exit path; no unbounded loops without cancellation

## Rationale

A leaked goroutine is a memory leak that does not show up in unit tests but compounds in production. Unlike allocated memory, the garbage collector cannot reclaim a running goroutine. Leaked goroutines also hold references to their closure variables, channels, and connections — the blast radius grows over time.

## Details

- Every go func() needs a clear exit. All blocking ops: select with ctx.Done().
- No for{}/for range ch without cancellation. Bind lifetime via errgroup.
- Tickers: defer Stop(), select ctx.Done(). Channels: handle close (ok idiom).
- Reject: missing ctx.Done(); unstoppable ticker; no-timeout blocking; goroutine outliving handler.

