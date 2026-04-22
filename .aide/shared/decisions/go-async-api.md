---
topic: go-async-api
decision: Use AsyncAPI specs as the contract for event-driven microservices; hand-write specs and validate in CI
decided_by: blueprint:go@0.0.59
date: 2026-04-21
references:
  - https://www.asyncapi.com/
  - https://www.asyncapi.com/docs/tools/cli
---

# go-async-api

**Decision:** Use AsyncAPI specs as the contract for event-driven microservices; hand-write specs and validate in CI

## Rationale

AsyncAPI is the event-driven equivalent of OpenAPI — it documents channels, messages, schemas, and protocol bindings for async APIs (NATS, Kafka, AMQP, MQTT, WebSocket). Spec-first design ensures producers and consumers agree on the contract before implementation.

## Details

- AsyncAPI 3.0 specs in YAML. Hand-written contracts (no Go auto-gen). Share JSON Schema with Go structs.
- CI: asyncapi validate ./asyncapi.yml. Docs: asyncapi generate.
- Keep protocol bindings (NATS subjects, Kafka topics) explicit in spec.
- Pair with Protobuf/Avro for cross-service schema evolution. Event-driven only.

