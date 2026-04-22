---
topic: lobslaw-tracing-library
decision: "go.opentelemetry.io/otel with the OTLP exporter. Spans at turn boundary, around LLM calls, around tool invocations, around memory operations. Exporter pluggable via [observability] config - OTLP default, stdout for dev, none to disable. Metrics via the same OTel SDK (OTLP → Prometheus via collector if needed). Rejected: direct prometheus/client_golang (tying metrics to Prom up-front loses format portability); no observability layer at all (workable for a single user but painful once you have multiple channels and scheduled tasks firing concurrently)"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-tracing-library

**Decision:** go.opentelemetry.io/otel with the OTLP exporter. Spans at turn boundary, around LLM calls, around tool invocations, around memory operations. Exporter pluggable via [observability] config - OTLP default, stdout for dev, none to disable. Metrics via the same OTel SDK (OTLP → Prometheus via collector if needed). Rejected: direct prometheus/client_golang (tying metrics to Prom up-front loses format portability); no observability layer at all (workable for a single user but painful once you have multiple channels and scheduled tasks firing concurrently)

## Rationale

OTel is the CNCF standard; treating it as the one observability dependency avoids choosing metrics and tracing libraries separately. OTLP out means any collector (Jaeger, Tempo, Datadog, Honeycomb, Grafana Cloud, local file) works. For personal-scale use the overhead is invisible; for debugging concurrent agent work it's invaluable

