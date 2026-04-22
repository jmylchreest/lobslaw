---
topic: go-ci-caching
decision: "Use setup-go cache: true for Go module and build caching; no separate cache step needed"
decided_by: blueprint:go-github-actions@0.0.59
date: 2026-04-21
references:
  - https://github.com/actions/setup-go/blob/main/docs/advanced-usage.md
---

# go-ci-caching

**Decision:** Use setup-go cache: true for Go module and build caching; no separate cache step needed

## Rationale

actions/setup-go with cache: true automatically caches both GOMODCACHE and GOCACHE, keyed on go.sum. This eliminates the need for a separate actions/cache step for most Go projects and keeps workflows simple.

## Details

setup-go with go-version-file: go.mod, cache: true. Caches GOMODCACHE (keyed on go.sum) and GOCACHE (go.sum + workflow). Monorepo: set cache-dependency-path to list all go.sum files. Only add separate actions/cache for extra artifacts (e.g., tree-sitter builds, generated code).

