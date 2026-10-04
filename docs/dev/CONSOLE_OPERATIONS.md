# Shared console operations

`internal/console` is the application boundary for browser and peer operations.
It has no HTTP, gRPC, compute or memory-store imports. Node wiring supplies the
existing bot, group, inbox, session, routine, memory, plan, prompt and learned-review
services through narrow interfaces. The gateway keeps compatibility aliases for
existing consumers; they do not create new stores or registries.

HTTP authenticates cookies/JWTs and applies CSRF protection before decoding an
operation. Shared operations accept verified claims and a context, derive the
owner, and enforce resource ownership and revision checks themselves. Calling an
operation directly therefore cannot bypass a guard in a router. Peer transport
must separately authenticate a node certificate and validate the forwarded user;
a node certificate alone never establishes a resource owner.

The shared layer preserves the public data projections rather than returning
storage records wholesale. Bot PATCH retains omitted/empty/false distinctions.
Task approvals and learned review retain their existing human-facing services,
separate from execution claims and model authoring. Prompt operations recheck the
recipient and expiry; the legacy unauthenticated local development path remains
an explicit HTTP-only behavior and is never available to forwarded peers.

Domain inbox filters, ownership rules and queue errors live in `internal/bots`.
The memory service retains compatibility aliases for existing callers. Revision
conflicts implement `errors.Is(err, types.ErrConflict)` while preserving existing
Raft error behavior. Prompt storage is supplied as an interface and shares neutral
prompt sentinels with the turn layer. Gateway production files must not import
memory or BoltDB; an architecture regression test enforces that boundary.

## Delivery stack based on PR #348

1. Shared operations and removal of gateway storage dependencies (this change).
2. Replace the backend's synthetic HTTP dispatch with direct typed calls,
   including the streaming chat path. Preserve peer identity, cancellation and
   uncertain mutation outcomes; do not add automatic mutation retries.
3. Add an explicit build without the embedded web UI, with separate supported,
   enabled and available capability states. Explicit remote backends never fall
   back to local compute.
4. Generate browser contracts from the existing protobuf/Buf workflow while
   retaining intentional REST/config projections and exact revision values.

REST and gRPC now call the shared operations directly. Explicit protobuf
projections retain integer precision and optional field presence without a
protobuf/JSON round trip. Peer failures map domain errors to gRPC codes, including
cancellation and revision conflicts. The browser adapter performs only its own
HTTP decoding/encoding and calls a mutation once; an uncertain outcome is not
automatically retried.

Chat uses shared, transport-independent conversation routines in the gateway,
alongside the existing turn runner, conversation cache, gate and prompt services.
REST handles authentication, CSRF, uploads and SSE; peers supply verified user
claims and a typed event sink. Both reuse the same history/approval/resume
lifecycle. Stream send failure cancels the runner context, and progress goroutines
finish before the adapter returns. Task evidence retains its generated JSON
contract. No fake HTTP request, response writer, or backend handler dispatch
remains.

Tests cover direct operation ownership, unowned resources, stale revisions,
patch presence and large revisions, cancellation, prompt ownership and expiry,
stream failure, and existing local/remote console behavior.

## Building with or without the browser UI

A normal Go build supports the embedded UI. Run `make web` before compiling
to include its assets; set `ui-web` in the node functions to serve it. Missing
assets produce a diagnostic while APIs and other node functions keep running.

`make build-no-web` (or `go build -tags no_web ./...`) excludes the embed
directive, assets and node SPA mounting code, even if an earlier web build left
assets in the tree. It requires no Node/npm toolchain. `make test-no-web` runs
the Go suite for this variant. The binary still contains the same external
REST and cluster mTLS gRPC APIs and uses the same application services.

For containers, use `make docker-no-web` or
`docker build --build-arg WEB_VARIANT=no-web -t lobslaw:no-web .`.
The default `WEB_VARIANT=with-web` builds the existing UI. BuildKit skips the
Node/npm stage entirely for the no-web variant.

Capability discovery distinguishes `supported` (compiled into this binary),
`enabled`/`configured` (runtime intent), and `available` (mounted/usable).
Requesting `ui-web` in a no-web binary logs an explicit warning, preserves
that intent in discovery and keeps the HTTP API running without an SPA.

An explicit `[ui-web].backend` always takes precedence over local compute,
including in a no-web binary. Unavailability is reported; it never causes a
silent switch to local state or retries an uncertain mutation.
