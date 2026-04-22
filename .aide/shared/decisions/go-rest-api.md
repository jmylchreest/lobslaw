---
topic: go-rest-api
decision: "For REST APIs, use an OpenAPI spec-first approach; prefer huma with chi or stdlib net/http enhanced routing depending on complexity"
decided_by: blueprint:go@0.0.59
date: 2026-04-21
references:
  - https://huma.rocks/
  - https://github.com/danielgtaylor/huma
  - https://github.com/go-chi/chi
---

# go-rest-api

**Decision:** For REST APIs, use an OpenAPI spec-first approach; prefer huma with chi or stdlib net/http enhanced routing depending on complexity

## Rationale

OpenAPI spec-first design ensures API documentation stays in sync with implementation. For complex APIs, huma generates OpenAPI 3.1 specs from Go types with automatic validation and RFC 7807 errors. For simple APIs, stdlib net/http with its enhanced routing (method+path patterns) is sufficient and has zero dependencies.

## Details

- huma (danielgtaylor/huma) + humachi adapter. Structs with tags auto-generate OpenAPI 3.1. Type-safe handlers, JSON Schema validation, RFC 7807 errors.
- Simple APIs: stdlib net/http patterns. Add chi for middleware/route groups.
- Never gorilla/mux (maintenance mode). Avoid gin/echo.

