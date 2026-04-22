---
topic: go-ci-workflow
decision: "CI runs lint, vet, test with race detector, module verification, and vulnerability scan on every PR"
decided_by: blueprint:go-github-actions@0.0.59
date: 2026-04-21
references:
  - https://github.com/actions/setup-go
---

# go-ci-workflow

**Decision:** CI runs lint, vet, test with race detector, module verification, and vulnerability scan on every PR

## Rationale

Each check catches a different class of bug. Linting catches style and correctness issues. Vet catches suspicious constructs. Race detector catches data races invisible to normal testing. Module verification catches supply chain tampering. Vulnerability scanning catches known CVEs in dependencies.

## Details

ci.yml (PR + push to main). Three parallel jobs: 1) lint: golangci-lint v2, 2) test: go mod verify, go vet, go test -race -coverprofile=coverage.out -covermode=atomic ./..., 3) vuln: govulncheck-action. Config: go-version-file: go.mod, cache: true, fetch-depth: 1. Upload coverage artifact. Build job depends on lint+test.

