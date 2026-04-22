---
topic: go-testing
decision: "Table-driven tests with t.Parallel, t.Helper, race detector always on, testing/synctest for concurrency"
decided_by: blueprint:go@0.0.59
date: 2026-04-21
references:
  - https://go.dev/doc/tutorial/add-a-test
  - https://pkg.go.dev/testing
---

# go-testing

**Decision:** Table-driven tests with t.Parallel, t.Helper, race detector always on, testing/synctest for concurrency

## Rationale

Table-driven tests reduce duplication and make it easy to add cases. Parallel tests catch data races and reduce CI time. The race detector finds bugs that are invisible to normal testing. testing/synctest replaces fragile time.Sleep in concurrent tests.

## Details

- Table-driven with t.Run. t.Parallel() in each test/subtest. t.Helper() in test utils.
- Manual mocks via interfaces; generated only for large interfaces.
- testing/synctest for concurrency (fake clock). No time.Sleep.
- CI: go test -race ./... always. Benchmarks: b.ResetTimer(), b.ReportAllocs().
- Prefer stdlib assertions; testify optional.

