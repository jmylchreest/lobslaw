---
topic: lobslaw-proto-toolchain
decision: "bufbuild/buf for proto generation, linting, and breaking-change detection. buf.yaml + buf.gen.yaml in repo root; buf generate invoked via Makefile target; buf lint and buf breaking run in CI. Rejected: raw protoc (no built-in lint, no breaking-change detection, manual dep management); protoc-gen-go alone (ditto)"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-proto-toolchain

**Decision:** bufbuild/buf for proto generation, linting, and breaking-change detection. buf.yaml + buf.gen.yaml in repo root; buf generate invoked via Makefile target; buf lint and buf breaking run in CI. Rejected: raw protoc (no built-in lint, no breaking-change detection, manual dep management); protoc-gen-go alone (ditto)

## Rationale

Buf is effectively the modern default for Go proto workflows. buf lint catches style issues that would otherwise proliferate; buf breaking on PR prevents accidental wire-protocol breakage - critical for a cluster where different versions may coexist briefly during rolling restart. The tooling is well-maintained by Buf Technologies with a weekly release cadence

