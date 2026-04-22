---
topic: go-module-management
decision: Use go mod tidy after every dependency change; pin tool dependencies with go tool directives
decided_by: blueprint:go@0.0.59
date: 2026-04-21
references:
  - https://go.dev/ref/mod
---

# go-module-management

**Decision:** Use go mod tidy after every dependency change; pin tool dependencies with go tool directives

## Rationale

go mod tidy ensures go.mod and go.sum are consistent and minimal. The go tool directive (modern Go) replaces the tools.go blank-import hack with first-class support for tool dependencies tracked in go.sum.

## Details

- Module path must be a real import path: go mod init github.com/org/repo.
- Run go mod tidy after every dep change. Run go mod verify in CI.
- Tool deps (linters, generators): use go tool directives in go.mod, run with go tool <name>. No tools.go hack.
- Never vendor unless air-gapped/hermetic builds require it.

