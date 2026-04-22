---
topic: go-tooling
decision: "Use gofmt/goimports, the latest golangci-lint, go vet, and govulncheck as the standard toolchain"
decided_by: blueprint:go@0.0.59
date: 2026-04-21
references:
  - https://golangci-lint.run/
---

# go-tooling

**Decision:** Use gofmt/goimports, the latest golangci-lint, go vet, and govulncheck as the standard toolchain

## Rationale

gofmt eliminates all formatting debates. the latest golangci-lint runs dozens of linters in a single pass with a unified configuration. govulncheck catches known vulnerabilities in dependencies. These tools catch real bugs with minimal false positives.

## Details

Run: gofmt/goimports, go vet, golangci-lint v2, govulncheck, go test -race. .golangci.yml v2: version: "2", timeout 5m, default: standard + errcheck/gocyclo/revive/gosec/unconvert/unparam/misspell, gocyclo min 15, exclude comments+std-error-handling, relax in _test.go, formatters: gofmt+goimports. v2 needs version field, nested linters.settings.

