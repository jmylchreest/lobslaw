---
topic: go-error-handling
decision: "Always wrap errors with context using %w; use errors.Is/As for inspection; never return nil, nil"
decided_by: blueprint:go@0.0.59
date: 2026-04-21
references:
  - https://go.dev/blog/go1.13-errors
  - https://go.dev/doc/effective_go#errors
---

# go-error-handling

**Decision:** Always wrap errors with context using %w; use errors.Is/As for inspection; never return nil, nil

## Rationale

Wrapping preserves the error chain for diagnosis while adding context at each call site. errors.Is/As work with wrapped errors while direct comparison (==) breaks. Returning nil, nil for 'not found' forces callers to check for both nil cases and leads to subtle bugs.

## Details

- Wrap: fmt.Errorf("create user: %w", err). Inspect: errors.Is/As, never ==.
- Sentinels: var ErrNotFound = errors.New("not found"). Structured types for field extraction.
- Never nil, nil — use sentinel. errors.Join for multi-error.
- Never ignore errors (_ needs comment). No panic; recover at goroutine boundaries only.

