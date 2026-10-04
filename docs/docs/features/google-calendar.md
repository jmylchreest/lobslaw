---
title: Google Calendar
description: Configure Google OAuth, independent Calendar read/write permissions, and per-user account connections.
---

# Google Calendar

Google Calendar is an opt-in, first-party connector for every authenticated
lobslaw user. Each user connects their own Google account and selects calendars
with independent read and write grants. Event creation and updates always require
confirmation of the exact change. KitchenOwl, Drive and Gmail are outside this
release.

## 1. Configure the operator-managed OAuth client

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

| Key | Default | Meaning |
| --- | --- | --- |
| `enabled` | `false` | Register the connector and its tools when enabled. |
| `client_id_ref` | Empty | Required when enabled; `env:NAME` or `file:/path` reference to the Google client ID. |
| `client_secret_ref` | Empty | Required when enabled; secret reference to the Google client secret. |
| `callback_url` | Empty | Required when enabled; exact HTTPS URL ending in `/integrations/google/callback`, without query or fragment. |

Supply the referenced variables to the running lobslaw process, for example from
a Kubernetes Secret or a container environment file. Do not put credential values
in `config.toml`. Restart the node after changing connector configuration. This
uses `[security.google_calendar]`, not the legacy `[security.oauth.google]`
device-flow configuration. Each user grants their own Google account access;
the client ID and secret identify the deployment's OAuth application.

Terminate TLS at the gateway or a reverse proxy. The user's browser must reach
both `/integrations/google/start` and `/integrations/google/callback` at that
HTTPS origin. These two routes use short-lived state and a secure browser cookie;
they do not require a lobslaw bearer token. Do not log callback query strings,
which contain authorization codes. `/v1/calendar` always requires a valid bearer
token, even if the normal message gateway allows anonymous requests.

For a gateway serving plain HTTP inside Docker or Kubernetes, route those paths
through an HTTPS reverse proxy to its configured HTTP port. A tailnet-only
callback can remain private if the user's browser can reach it and Google accepts
the registered URI. Do not expose the gateway's other management routes merely
to make OAuth work.

Outbound calls use the dedicated `integration/google-calendar` egress role,
restricted to `www.googleapis.com`, `oauth2.googleapis.com` and
`openidconnect.googleapis.com`. Browser navigation to Google's consent screen
happens on the user's device. All connector HTTP calls are bounded, have a
30-second timeout, refuse redirects and omit provider response bodies from errors.

## 2. Allow reads and optionally writes

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

Example read-only policy for users with role `calendar-user`. Assign this role
through the existing [user identity/channel configuration](/configuration/channels)
or validated REST claims. For an owner-only deployment, replace
`subject = "role:calendar-user"` with `subject = "scope:owner"` in **every** rule
below. A policy role does not itself grant access to a chat channel.

Enable `[policy]` in your node configuration, then add these rules:

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
To permit writes with mandatory confirmation, add this **separate** rule:

```toml
[[policy.rules]]
id = "calendar-write"
subject = "role:calendar-user"
action = "calendar:write"
resource = "google/*"
effect = "allow"
priority = 20
```

Use the same subject as the other rules. User-level calendar grants can still
remain read-only. Existing broader operator rules also apply; review those
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

## 3. Connect an account in a private conversation

In a private Telegram conversation, the commands below are human management
commands, not instructions for the agent to run. Keep the returned flow ID;
it identifies this login attempt, whereas the connection ID identifies the
activated account. Replace angle-bracket placeholders with the displayed values:


1. `/calendar connect read` (or `write` to request event-write consent).
2. Open the returned short-lived URL and choose the Google account.
3. Return to chat and run `/calendar pending <flow-id>`.
4. Check the displayed account and calendar IDs, then run
   `/calendar allow <flow-id> <displayed-email> <calendar-id> read`.
   The final permission can be `read`, `write`, or `read-write`.
5. `/calendar list` shows connection IDs and grants. Tell the assistant which
   connection/calendar to use, then ask for your agenda or an event change.

For read/write use, start with `/calendar connect write` and finish with
`/calendar allow <flow-id> <displayed-email> <calendar-id> read-write`.
The Telegram activation command selects one calendar per connection. Use the
REST activation example below to select several calendars together.

After setup, ask “Using this connection and calendar, what is on tomorrow?”
or “Add a dentist appointment tomorrow from 10:00 to 11:00.” The second request
must show the exact change and require approval before it is sent to Google.

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

## Privacy and learning

Connections are bound to the authenticated user's canonical identity. Tokens stay
in encrypted connector storage and are not issued to skills. Shared-chat Calendar
access is refused. The self-learning review skips transcripts containing Calendar
tool calls, but ordinary conversation retention and model-provider handling still
apply to event content. This is not general data-loss prevention for later
paraphrases or summaries.

## Troubleshooting and first-use checks

| Symptom | Check |
| --- | --- |
| `/calendar` is unavailable | Confirm the deployed version includes Calendar, the connector is enabled, and the node has compute plus local memory/policy services and an enabled gateway. |
| Command or operation denied | Check channel access, caller scope/roles, `command:exec`, `tool:exec`, and the independent `calendar:*` rules. Operation rules must use `allow`; confirmation is a separate write gate. |
| Google rejects the redirect | Match the configured HTTPS callback exactly to the Google Web application client's registered redirect URI. |
| Connection failed or expired | Ensure the same browser retains the secure cookie, complete the flow within 15 minutes, or start a new attempt. |
| Reads work but writes fail | Check operator `calendar:write`, Google write consent, and the selected calendar's local write grant. Updates also require reads. |
| Event changed since review | Start a new turn to fetch the current version and approve the new change. |
| Write outcome uncertain | Inspect Google Calendar before requesting another change; the connector will not automatically resend it. |
| Need different calendars or permissions | Disconnect and reconnect; there is no in-place grant-edit command yet. |

Start with a read-only connection and confirm that an event-write request is
refused. Then, if wanted, reconnect with read/write grants, create a disposable
guest-free event, and check that no write occurs until you approve its exact
preview. Deny another change and verify the event stays unchanged. Finally test
an update and disconnect. This live OAuth/account smoke test complements the
automated tests, which use fake Google responses.
