# Google Calendar

Google Calendar is an opt-in, first-party connector for every authenticated
lobslaw user. Each user connects their own Google account and selects calendars
with independent read and write grants. Event creation and updates always require
confirmation of the exact change. KitchenOwl, Drive and Gmail are outside this
release.

## Operator setup

Run the connector on a node with compute, local memory/policy services, and an
enabled HTTP gateway. Upgrade every Raft peer before enabling it: this feature
adds credential ownership fields, replicated integration state, and durable encrypted connector settings.
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
resource = "calendar*"
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
| `calendar_settings` | read | Own local calendar preferences and nicknames |
| `calendar_settings_update` | write + network | Exact confirmation for availability, defaults, timezone, access and disconnect; rename/agenda offer Undo |
| `calendar_agenda` | read + network | Combined view with source identities and explicit partial failures |
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

In a private Telegram conversation, open `/calendars`:

1. Choose **Connect Google (read)** or **Connect Google (read/write)**.
2. Open the short-lived link and choose your Google account.
3. Return and tap **Choose calendars**. Each calendar cycles through off, read,
   and (when available) read/write. Selections can span pages.
4. Tap **Review selection**, check the account and each calendar's grants, then
   confirm. Repeat to connect another Google account.
5. Give calendars nicknames and purposes in chat. `/calendars` also provides
   buttons for agenda/availability inclusion, access, the general default, and
   disconnecting an account connection.

Examples of normal chat requests:

- “Show my calendars.”
- “Call my personal calendar Home and my shared calendar Family.”
- “Use Family for school events and Home as my general default.”
- “Include Work when checking availability, but leave it out of my agenda.”
- “Make Work read-only.”
- “My time zone is Europe/London.”
- “What's on tomorrow across my calendars?”
- “Add the school concert to Family on Friday at 6pm.”

Nicknames are unique per canonical lobslaw user, across connected accounts.
They are local labels; renaming one does not rename the Google calendar. Purposes
are explicit labels chosen by the user, such as `general`, `personal`, or `school`.
When creating an event, the assistant can select a nickname or a purpose default.
Missing or ambiguous destinations require a choice; the service never guesses a
calendar. Updates require the calendar containing the event as well as its ID.

Rename and agenda inclusion apply immediately and return an Undo action. Undo
expires after 15 minutes and is available only until another settings change.
Availability inclusion, purpose defaults, time zone, access changes and disconnects
show the exact proposed change for human confirmation. Preferences are durable
user settings, not learned skills or conversation memory. Read and write remain
independent; write-only can be requested in chat or through the API. Creation
works with write-only access, while updating an existing event also needs reads.

The combined agenda retains each calendar's nickname and account. Availability
mode reads the calendars selected for availability; it returns events, not a
provider free/busy guarantee. Any failed or truncated calendar makes the result
incomplete. The assistant must not conclude that a slot is free from incomplete
results. Queries cover at most 25 included calendars and have a two-minute overall
timeout. Narrow the interval when individual calendars exceed 100 events.

To connect more calendars from an existing Google account, repeat onboarding and
select them; do not duplicate existing calendars unless you want duplicate agenda
results. Each connection is managed independently. Adding Calendar access cannot
grant Drive or Gmail access.

The legacy `/calendar` commands remain available for existing clients.

OAuth completion alone does not activate a connection. Activation is a human
management operation, absent from agent tools. A Google write token does not
imply a local write grant. Write-only grants permit creation; updates additionally
need read access to inspect the event before confirmation.

`/calendar disconnect <connection-id>` removes local credentials immediately and
invalidates pending changes that use them. Remove the application's access in
Google Account settings to revoke Google's grant as well. To add unselected calendars, connect again. Existing selected-calendar grants can be changed in chat; increasing access checks Google scope, calendar role and operator policy. A read-only Google token needs reconnection before enabling writes. Account links expire after
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

Additional authenticated API operations:

- `inventory`: calendar IDs, nicknames, independent grants, inclusion flags,
  defaults, time zone and settings revision.
- `settings_prepare`: accepts `change` with `operation`, `calendar` (nickname or
  inventory ID), and operation-specific fields: `value`, `enabled`, or
  `permission: {"read":true,"write":false}`. Optional `revision` rejects stale UI.
- `settings_apply`: accepts the prepared `id`; this is an authenticated **human
  confirmation** endpoint, never an agent tool.
- `settings_immediate`: applies only rename, agenda and valid Undo changes;
  rejects operations requiring confirmation.

For example, prepare `{"operation":"settings_prepare","change":{"operation":
"default","calendar":"Family","value":"school"}}`, display the returned summary,
then submit `{"operation":"settings_apply","id":"PREPARED_ID"}` only after the
user confirms it. Use `calendar_settings_update` for natural-language agent work;
its trusted execution gate owns confirmations and the model cannot call approval.

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
owner, settings revision, credential generation and current event ETag in encrypted replicated state.
The approval resource includes a nonce, so an expired/recreated preparation cannot
borrow an earlier confirmation. A changed argument produces a different operation.
Previews larger than the safe chat review limit are rejected, never truncated.

On resume, ownership, selected grants, operator policy and credential generation
are checked again. Settings changes invalidate outstanding event approvals. Updates reread the event and send `If-Match`; a stale version
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
Durable preferences live in a separate encrypted `integration_settings` bucket,
keyed by canonical user and connector. CAS protects concurrent nickname/default
changes. They do not expire with OAuth flows, and cannot be read through memory
or skill APIs. Portable memory archives omit credentials, integration settings and integration state; physical
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
