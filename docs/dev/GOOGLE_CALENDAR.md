# Google Calendar

Google Calendar is an opt-in, first-party connector for every authenticated
lobslaw user. Each user connects their own Google account and selects calendars
with independent read and write grants. Event creation and updates always require
confirmation of the exact change. KitchenOwl, Drive and Gmail are outside this
release.

## Operator setup

Run the connector on a node with compute, local memory/policy services, and an
enabled HTTP gateway. Upgrade every Raft peer before enabling it: this feature
adds credential ownership fields and a replicated integration-state record.
Older binaries must not serve credentials from an upgraded store.

Create a Google OAuth **Web application** client, enable the Calendar API, and
register the exact HTTPS callback URI. Configure the consent screen for your
intended users. A public application may need Google's verification process;
a testing application must list its test users. See Google's
[web-server OAuth guide](https://developers.google.com/identity/protocols/oauth2/web-server)
and [Calendar scopes](https://developers.google.com/workspace/calendar/api/auth).

```toml
[security.google_calendar]
enabled = true
client_id_ref = "env:LOBSLAW_GOOGLE_CALENDAR_CLIENT_ID"
client_secret_ref = "env:LOBSLAW_GOOGLE_CALENDAR_CLIENT_SECRET"
callback_url = "https://assistant.example.com/integrations/google/callback"
```

Terminate TLS at the gateway or a reverse proxy. The user's browser must reach
both `/integrations/google/start` and `/integrations/google/callback` at that
HTTPS origin. These two routes use short-lived state and a secure browser cookie;
they do not require a lobslaw bearer token. Do not log callback query strings,
which contain authorization codes. `/v1/calendar` always requires a valid bearer
token, even if the normal message gateway allows anonymous requests.

The homelab deployment inspected during development runs a single StatefulSet
with a tailnet-only gateway on port 8443 and plain HTTP internally. It therefore
needs HTTPS termination and routing for the browser callback. A private callback
can remain on the tailnet if the user's browser can reach it and Google accepts
the registered URI. This feature does not change that deployment or its secrets.

Outbound calls use the dedicated `integration/google-calendar` egress role,
restricted to `www.googleapis.com`, `oauth2.googleapis.com` and
`openidconnect.googleapis.com`. Browser navigation to Google's consent screen
happens on the user's device. All connector HTTP calls are bounded, have a
30-second timeout, refuse redirects and omit provider response bodies from errors.

## Policy and classification

Permissions have three layers, all of which must allow the operation:

1. Operator policy permits the tool and the operation action.
2. The user selected that calendar and granted the corresponding access.
3. Google granted the required OAuth scope and permits access to that calendar.

The operation actions are `calendar:connect`, `calendar:read`, and
`calendar:write`. `calendar:connect` uses resource `google`; event actions use
`google/<connection-id>/<URL-path-escaped-calendar-id>`. Listing selected calendars
uses `google/<connection-id>/*`. No matching policy means deny. These actions
require an explicit `allow`; a `require_confirmation` policy is treated as denied.
Calendar writes have a separate mandatory exact confirmation after authorization,
so an operator allow cannot suppress that confirmation.

Example read-only policy for users with role `calendar-user`:

```toml
[[policy.rules]]
id = "calendar-account-management"
subject = "role:calendar-user"
action = "calendar:connect"
resource = "google"
effect = "allow"
priority = 20

[[policy.rules]]
id = "calendar-read"
subject = "role:calendar-user"
action = "calendar:read"
resource = "google/*"
effect = "allow"
priority = 20

[[policy.rules]]
id = "calendar-tools"
subject = "role:calendar-user"
action = "tool:exec"
resource = "calendar_*"
effect = "allow"
priority = 20

[[policy.rules]]
id = "calendar-command"
subject = "role:calendar-user"
action = "command:exec"
resource = "calendar"
effect = "allow"
priority = 20
```

The tool wildcard does **not** grant writes: `calendar:write` remains denied.
Add a separate rule for that action when desired. User-level calendar grants can
still remain read-only. Existing broader operator rules also apply; review those
when configuring a read-only role.

Trusted tool definitions declare state effects separately from network transport:

| Tool | Effects | Additional requirements |
| --- | --- | --- |
| `calendar_list` | read | Own selected readable calendars; listing is local |
| `calendar_events` | read + network | Explicit bounded start/end interval |
| `calendar_event` | read + network | Exact event ID |
| `calendar_event_create` | write + network | Exact confirmation |
| `calendar_event_update` | read + write + network | Read access for preview, version check, exact confirmation |

Inspect these declarations without executing anything:

```sh
lobslaw policy classify --tool calendar_events --json
lobslaw policy classify --tool calendar_event_update --json
```

The classifier preserves independent effects; adding `network` or `write` does
not erase a real `read`. Tool effects describe operations, not authorization.
Shell/session label grants and approvals of another tool or payload cannot satisfy
the Calendar write gate.

## User setup

In a private Telegram conversation:

1. `/calendar connect read` (or `write` to request event-write consent).
2. Open the returned short-lived URL and choose the Google account.
3. Return to chat and run `/calendar pending <flow-id>`.
4. Check the displayed account and calendar IDs, then run
   `/calendar allow <flow-id> <displayed-email> <calendar-id> read`.
   The final permission can be `read`, `write`, or `read-write`.
5. `/calendar list` shows connection IDs and grants. Tell the assistant which
   connection/calendar to use, then ask for your agenda or an event change.

OAuth completion alone does not activate a connection. Activation is a human
management operation, absent from agent tools. A Google write token does not
imply a local write grant. Write-only grants permit creation; updates additionally
need read access to inspect the event before confirmation.

`/calendar disconnect <connection-id>` removes local credentials immediately and
invalidates pending changes that use them. Remove the application's access in
Google Account settings to revoke Google's grant as well. To change selected
calendars or permissions, disconnect and reconnect. Account links expire after
15 minutes. If a login is interrupted after its code was consumed, start a new
connection attempt. `/calendar list` resolves uncertainty after activation.

Other clients use authenticated `POST /v1/calendar` with a JSON object:

```json
{"operation":"connect","write":false}
```

Operations are `connect`, `pending` (with `id`), `activate` (with `id`, the displayed
`email`, and `calendars`), `list`, and `disconnect` (with connection `id`). Example
activation, including multiple selected calendars:

```json
{"operation":"activate","id":"FLOW_ID","email":"you@example.com","calendars":{"CALENDAR_ID":{"read":true,"write":false}}}
```

Identity always comes from validated claims and the canonical identity resolver,
never a request field. Large account listings use this API instead of truncating
chat output. Shared-chat management and Calendar tool use are refused.

## Event behavior and confirmation

Reads return at most 100 expanded occurrences in a window of at most 366 days.
`truncated: true` means ask for a narrower interval. Pagination/sync cursors and
background watches are not implemented. Initial account selection supports at
most 250 subscribed calendars; larger accounts currently need fewer subscriptions.

Create/update accept only title, start, end, description and location, plus the
connection/calendar and (for update) event ID. Timed events require RFC3339 offsets;
optional time zones must be valid IANA names. All-day dates use an exclusive end.
Updates require title/start/end; omitted description/location are preserved.
No arbitrary provider JSON, attendees, deletes, moves, cancellations, recurrence
rules, series edits or special event types are accepted. An individual expanded
recurring occurrence may be updated by its own ID.

The gate runs after parameter-changing hooks. It persists the exact typed change,
owner, credential generation and current event ETag in encrypted replicated state.
The approval resource includes a nonce, so an expired/recreated preparation cannot
borrow an earlier confirmation. A changed argument produces a different operation.
Previews larger than the safe chat review limit are rejected, never truncated.

On resume, ownership, selected grants, operator policy and credential generation
are checked again. Updates reread the event and send `If-Match`; a stale version
requires a new turn and confirmation. See Google's
[resource version guide](https://developers.google.com/workspace/calendar/api/guides/version-resources).
A Raft compare-and-swap marks the operation dispatched **before** sending a write,
preventing concurrent resumes from transmitting duplicates. Creation uses a
stable provider event ID derived from owner, turn and payload.

A timeout or crash after dispatch leaves the outcome uncertain. No automatic retry
or reconciliation is implemented: inspect the event in Google Calendar (or through
a permitted read) before asking for a new change. Successful receipts allow repeats
within the state lifetime to return the result without writing again.
Writes request `sendUpdates=none`; this is not a promise of complete silence from
Google or invisibility to people who already share the calendar. See
[Events.insert](https://developers.google.com/workspace/calendar/api/v3/reference/events/insert).

## Credentials, learning and future services

Connector credentials carry a canonical owner and a connector binding. Legacy
OAuth status, skill issuance, grant/revoke and credential replacement paths cannot
read or modify them, including a skill named `google-calendar`. The FSM rejects
ownership/connector changes from stale proposals. Tokens and connection metadata
are encrypted before entering Raft; refresh reuses the existing coordinated
credential refresh mechanism. Google `sub` identifies the external account;
email is only display/confirmation metadata. Legacy unowned credentials are not
adopted automatically.

OAuth state, PKCE verifier, pending tokens and mutation previews are encrypted and
replicated in `integration_state`. Replays and concurrent transitions use CAS.
Expired records are removed by the leader's reaper. Allocation is bounded to 100
records per owner and 10,000 globally, including records awaiting cleanup.
Portable memory archives omit credentials and integration state; physical
snapshots contain their encrypted records and require the cluster key.

Learned instructions may describe how to use the typed tools, but cannot expand
permissions, activate accounts, approve changes or obtain tokens. Calendar text
is untrusted tool data. The post-turn self-learning fork skips transcripts that
contain Calendar tool calls. Tool descriptions also instruct the model not to
save event contents in learned notes/skills. This is not general data-loss
prevention: later user paraphrases or summaries without tool provenance are not
identified by that guard. Existing conversation retention and model-provider data
handling still apply to event content returned to the assistant.

Drive and Gmail should add separate connectors, OAuth consent and operation
permissions. Do not extend a Calendar token and give it to skills: a local list
of allowed scopes does not downscope a bearer token at Google. Provider-managed
scope granularity and lobslaw's operation permissions are separate boundaries.

Tests use fake Google transports and real local Raft/storage fixtures. They cover
account/owner binding, replay, cookie/PKCE handling, independent read/write grants,
exact approval, concurrency, version conflicts, credential replacement, redirects,
response bounds, private management and classification. Live Google consent and
an actual account round trip remain deployment smoke tests.
