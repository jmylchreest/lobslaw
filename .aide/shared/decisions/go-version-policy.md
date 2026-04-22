---
topic: go-version-policy
decision: Always target the latest stable Go version; use the go directive in go.mod as the single source of truth
decided_by: blueprint:go@0.0.59
date: 2026-04-21
references:
  - https://go.dev/doc/devel/release
  - https://go.dev/dl/
---

# go-version-policy

**Decision:** Always target the latest stable Go version; use the go directive in go.mod as the single source of truth

## Rationale

Go maintains excellent backwards compatibility. Staying current ensures access to security fixes, performance improvements, and new language features. The Go team supports the two most recent major releases. The go directive in go.mod is read by tooling (setup-go, IDE, CI) so it eliminates version drift.

## Details

- New modules: go mod init with current stable version.
- Existing modules: bump go directive in go.mod to latest stable.
- CI: use go-version-file: go.mod, never hardcode versions.
- Check https://go.dev/dl/ for current stable.

