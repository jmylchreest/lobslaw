---
topic: go-stdlib-preference
decision: Prefer modern standard library features over third-party equivalents
decided_by: blueprint:go@0.0.59
date: 2026-04-21
references:
  - https://go.dev/doc/devel/release
---

# go-stdlib-preference

**Decision:** Prefer modern standard library features over third-party equivalents

## Rationale

The Go standard library is high quality, stable, and has zero dependency cost. Modern Go has absorbed many features that previously required third-party libraries. Fewer dependencies mean less supply chain risk and simpler maintenance.

## Details

Prefer stdlib: log/slog over logrus/zap/zerolog, errors.Join over multierr, context.WithoutCancel over Background() hack, net/http patterns over gorilla/mux, iter.Seq/Seq2 over manual iterators, testing/synctest over time.Sleep, min/max builtins, slices/maps packages. Migrate deps when stdlib catches up.

