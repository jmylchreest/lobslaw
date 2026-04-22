---
topic: go-logging
decision: Use log/slog from the standard library; structured key-value pairs; no third-party logging libraries
decided_by: blueprint:go@0.0.59
date: 2026-04-21
references:
  - https://pkg.go.dev/log/slog
---

# go-logging

**Decision:** Use log/slog from the standard library; structured key-value pairs; no third-party logging libraries

## Rationale

log/slog is the official structured logging package in Go's standard library. It eliminates the need for logrus, zap, or zerolog while providing a standard interface that libraries and applications can share.

## Details

- slog only. No logrus/zap/zerolog. Structured KV: slog.Info("msg", "key", val). Enrich with slog.With(...).
- Libraries: never slog.SetDefault(); accept *slog.Logger or use slog.Default().
- Use InfoContext/ErrorContext for trace propagation.
- Debug=dev, Info=ops, Warn=recoverable, Error=actionable. Never log Error and return err.

