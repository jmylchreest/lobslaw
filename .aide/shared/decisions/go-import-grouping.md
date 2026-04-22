---
topic: go-import-grouping
decision: "Three import groups: stdlib, third-party, internal — enforced by goimports"
decided_by: blueprint:go@0.0.59
date: 2026-04-21
---

# go-import-grouping

**Decision:** Three import groups: stdlib, third-party, internal — enforced by goimports

## Rationale

Consistent import grouping makes dependency relationships visible at a glance and is enforced automatically by goimports.

## Details

Three groups separated by blank lines: 1) stdlib 2) third-party 3) internal (same module). Example: import ("context" / "github.com/go-chi/chi/v5" / "github.com/org/repo/internal/service"). goimports handles this automatically; include in editor save hook and golangci-lint formatters.

