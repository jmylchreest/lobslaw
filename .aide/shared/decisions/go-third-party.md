---
topic: go-third-party
decision: "Justified third-party library choices for common needs; prefer narrow, well-maintained libraries over frameworks"
decided_by: blueprint:go@0.0.59
date: 2026-04-21
---

# go-third-party

**Decision:** Justified third-party library choices for common needs; prefer narrow, well-maintained libraries over frameworks

## Rationale

Go's stdlib covers most needs but some problem domains benefit from proven third-party solutions. Each dependency should be justified by clear value over rolling your own. Prefer libraries with narrow scope, active maintenance, and significant community adoption.

## Details

Approved: chi (complex routing), huma/v2 (OpenAPI), cobra (CLI), viper or caarlos0/env (config), pgx/v5 (Postgres), sqlc (SQL gen over ORMs), go-playground/validator, go-retryablehttp (HTTP retries), x/sync/errgroup. Avoid GORM. Every dep must clearly beat stdlib.

