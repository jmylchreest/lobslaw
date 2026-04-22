---
topic: go-project-structure
decision: Use cmd/internal/pkg layout for multi-binary projects; flat layout for simple projects
decided_by: blueprint:go@0.0.59
date: 2026-04-21
references:
  - https://go.dev/doc/modules/layout
---

# go-project-structure

**Decision:** Use cmd/internal/pkg layout for multi-binary projects; flat layout for simple projects

## Rationale

The cmd/internal/pkg convention is well-understood in the Go community. internal/ provides compiler-enforced encapsulation. But adding structure prematurely increases complexity without benefit.

## Details

- cmd/<name>/main.go: thin entry (~50 lines). internal/: private logic. pkg/: only when another module imports it today.
- Flat layout fine for simple projects; add structure when complexity warrants.
- Package names: short, lowercase, no underscores/utils/helpers/common.

