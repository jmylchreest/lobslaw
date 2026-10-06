# Resumable console chat

The browser creates a server-owned turn with `POST /v1/chat-turns`. Refreshing,
closing the tab, mobile suspension, or losing a polling connection does not cancel
execution. The console finds the latest turn for its conversation on reopening and
polls its retained state. Bot chat and the single-assistant console use the same
lifecycle. Legacy `/v1/messages` and bot SSE endpoints retain their existing
request-bound behavior for other clients.

## HTTP contract

- `POST /v1/chat-turns`: `{id, bot, message}` or `{id, session_id, message}` for
  single-assistant chat. Returns `202` with the retained turn. An identical request
  ID under the same authenticated owner is idempotent; changed content conflicts.
- `GET /v1/chat-turns?bot=<id>`: latest retained turn for this owner and bot.
  Single-assistant chat uses `session_id=console` and an empty bot.
- `GET /v1/chat-turns/<id>`: state, latest event, public payload, timestamps and
  submitted message. States are running, waiting, completed, failed, cancelled,
  and interrupted.
- `DELETE /v1/chat-turns/<id>`: explicit Stop. Cancels the runner and retains a
  cancellation receipt; already performed external effects are not undone.

Authentication and cookie CSRF checks apply to every route. IDs are scoped to the
authenticated owner. Bot ownership is checked on submission and retrieval,
including through `QueryConsole` on remote backends. Browser logout cancels its
outstanding execution; credential expiry and gateway shutdown also bound it.
Network reconnects never resubmit a message. A lost creation response can safely
be retried with the identical request ID.

## Persistence and bounds

When browser-session persistence is configured, the gateway stores encrypted
turn records in `auth/chat-turns/` beside its browser-session file. A private
node-local journal key and atomic mode-0600 record writes protect the stored
messages/results. Creation is persisted before execution begins; results and
pending confirmations are persisted before being served to reconnecting clients.
The browser stores no transcript or bearer credential in localStorage.

The journal retains up to 128 records for 24 hours, at most 512 KiB per record,
with 16 concurrent turns globally and four per owner. There is one active turn
per owner/conversation and a 15-minute execution ceiling, additionally bounded
by credential expiry and existing runner budgets. Expired records are reaped on
admission or startup. Capacity failures happen before execution.

This journal belongs to the web gateway, including when execution is proxied to
another node through `ChatConsole`. Reconnect to the same gateway. It is not a
replicated work queue or cross-gateway failover mechanism.

On a gateway restart, completed results remain readable. Unfinished records are
marked interrupted and **never automatically executed again**: their external
effects may already have occurred. The console directs the person to inspect the
recorded conversation before starting new work. A backend disconnect is likewise
reported as an interrupted reply rather than a silent empty message.
