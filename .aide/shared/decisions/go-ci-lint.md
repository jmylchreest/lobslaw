---
topic: go-ci-lint
decision: "Use golangci-lint-action with version: latest to run the latest golangci-lint in CI"
decided_by: blueprint:go-github-actions@0.0.59
date: 2026-04-21
references:
  - https://github.com/golangci/golangci-lint-action
---

# go-ci-lint

**Decision:** Use golangci-lint-action with version: latest to run the latest golangci-lint in CI

## Rationale

golangci-lint-action handles installation, caching, and reporting. Using version: latest ensures you always get the latest the latest golangci-lint release. The .golangci.yml in the repo controls which linters run.

## Details

Step: uses: golangci/golangci-lint-action@<sha> with version: latest. Reads .golangci.yml from repo root (must have version: "2"). Permissions: contents: read only. Monorepo: run separately per module with working-directory.

