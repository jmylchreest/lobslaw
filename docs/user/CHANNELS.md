# Channels

lobslaw exposes the agent loop to users through **channels**. Today there are REST, an optional browser console, and Telegram. Channels are configured under `[[gateway.channels]]` in `config.toml`; you can mix and match.

## REST

Default. Mounts on the gateway HTTP port (8443 by default) at:

- `POST /v1/uploads` — upload image/audio bytes with a valid JWT; return an upload ID
- `POST /v1/messages` — send a message, get a reply
- `GET /v1/plan` — see what's scheduled and in-flight (401 when `require_auth` is on and you are not signed in)
- `GET /v1/prompts/{id}` / `POST /v1/prompts/{id}/resolve` — inspect or answer a confirmation
- `GET /v1/capabilities` — which surfaces this node has (does not grant access)
- `POST /v1/session` — exchange a JWT for a login cookie; `DELETE /v1/session` revokes it
- `GET /healthz`, `GET /readyz` — health probes (ungated)

A body field named `user_id` is ignored. Who you are comes from the Bearer token or the login cookie, resolved to `[[user]].id`. To enrol a browser user, declare them under `[[user]]` with `[[user.channels]] type = "rest"` and `address` equal to their JWT `sub`. Logging in does not make them an operator.

### Conversations over REST

By default each `POST /v1/messages` is independent — the agent starts from a blank thread every time. That's what you want for scripts and automations firing unrelated one-shot requests.

To hold an actual conversation, pass a `session_id`. Requests sharing one get the prior exchange replayed into the turn, and the transcript is stored in the cluster, so it survives restarts and node failover:

```bash
curl -X POST https://localhost:8443/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{"message": "my name is james", "session_id": "cli-42"}'

curl -X POST https://localhost:8443/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{"message": "what is my name?", "session_id": "cli-42"}'
```

Pick the id yourself — anything stable and unique per conversation, containing no `:` or `/` (both are rejected with a 400). Reusing an id resumes that conversation; a fresh id starts a new one.

Session ids are scoped to the authenticated caller, so two users who both pick `default` get two separate conversations and neither can read the other's. On a node with `require_auth = false` every caller is the same anonymous identity, and so shares one namespace — if REST is reachable by more than one person, authenticate it.

## Browser console

Off by default. `--all` does not turn it on. Enable it with `--ui-web` or:

```toml
[ui-web]
enabled = true
# Required when this node does not run compute: cluster gRPC of a compute node.
# backend = "compute-1:7443"

[auth]
require_auth = true
```

Then open the gateway HTTP port in a browser (8443 by default). Sign in with an enrolled JWT. To obtain a code from the CLI, set `LOBSLAW_LOGIN_TOKEN` to your enrolled JWT and run `lobslaw login --config <path>` (or supply `--token`). Type the six-digit code in the browser. Codes expire after five minutes and work once; guessing is limited to ten attempts per minute per web node. There is no self-signup: the person must already be in `[[user]]`. Assistants cannot mint sign-in codes, even in an operator's direct chat; use the CLI or browser so credentials never enter model context.

Loopback connections do not bypass authentication: a reverse proxy can make a remote browser appear to connect from localhost. The old **Continue on this computer** shortcut is no longer offered.

Browser sessions last 30 days and survive gateway/container restarts when
`[cluster].data_dir` is persistent. Each web node saves a private session snapshot
at `<data_dir>/auth/browser-sessions.json`; only hashes of the random cookie tokens
are stored. Logout is persisted, expired sessions are rejected, and current user
enrollment and roles are checked on each request. Sessions are local to the web
node, not replicated across web nodes. A node without a data directory uses
ephemeral sessions. Existing in-memory logins need one new sign-in after upgrading.

### Install on a phone

Serve the console at a stable **HTTPS** address with a certificate trusted by the
phone, either directly or through a TLS reverse proxy. A plain LAN HTTP address
does not support service workers or full PWA installation. Behind a proxy, forward
the original host and set `X-Forwarded-Proto: https` so login cookies are secure.

- **iPhone/iPad:** open in Safari, choose **Share → Add to Home Screen**.
- **Android:** choose **Install Lobslaw** in the console, or **Install app** in the
  browser menu. The installed app uses Lobslaw's name and home-screen icon.

The service worker caches the public application shell. An offline launch shows a
reconnection screen; conversations, API responses and credentials are not cached
by the worker, and messages/approvals are never queued for later replay. A network
outage is shown as a connection problem rather than a sign-out. Updates wait for
**Reload to update**, so finish active chats and save drafts before applying one.

### Selective push notifications

Choose **Enable notifications** in the signed-in console and grant the browser's
permission. On iPhone/iPad this must be done inside the installed Home Screen app
(iOS 16.4+). Preferences are per device; **Disable notifications** unsubscribes that
device. Logging out revokes subscriptions associated with that browser session.

Lobslaw sends push for approval/attention requests, failed work, explicit agent
`notify` calls, and completed routines configured with `notify_on=always`.
Ordinary replies and quiet routine completions do not generate push. Notifications
show the agent's name and open its conversation or the relevant task. The browser
uses the agent's cached avatar when available; OS presentation varies, and the
application identity remains Lobslaw. Only public avatar artwork is cached.

Subscriptions, VAPID credentials and delivery receipts survive restarts in the
web node's private `<data_dir>/auth/web-push.json`. Browser-vendor delivery uses
encrypted Web Push through the egress proxy, restricted to Google, Mozilla and
Apple push endpoints. Expired subscriptions are removed, transient failures back
off, and stable event tags coalesce retries. Phone delivery requires an active
subscription, HTTPS, and connectivity; this is not a guarantee of immediate OS
delivery. The web node polls durable bot inbox evidence, including through a
remote compute backend, every 15 seconds.

### Recurring agent work

With the scheduler and compute-teams enabled, ask a named agent to do work on a
schedule: "Every weekday at 9am Europe/London, check the assigned work and let me
know the outcome." The agent uses `schedule_create`, and must return its saved
routine id. Prompts are self-contained instructions, not inherited chat history.

Each occurrence enters that agent's durable task queue. Results and approvals
appear in its conversation even after closing the app. Replaying the same
occurrence does not create another task; an outstanding occurrence suppresses
overlapping runs. Missed intervals are coalesced by the scheduler rather than
replaying an entire outage's backlog. Runs retain the original human owner and
recheck the bot and current roles before execution.

- `schedule_list` / `schedule_get`: inspect instructions, timing, last dispatched
  inbox item, errors and checkpoint.
- `schedule_update`: pause/resume, edit instructions/timing, or save a bounded
  checkpoint for the next occurrence. The next run receives that checkpoint and
  an excerpt of the previous outcome; full evidence remains in the inbox.
- `schedule_delete`: stop future occurrences. Already queued work has its own
  cancellation controls.
- `notify_on=always`: announce each completed outcome, when the user requests it.
- `notify_on=match` (default): keep ordinary outcomes quiet; the agent can call
  `notify` for a meaningful change or a question needing attention.
- `notify_on=never`: quiet outcomes. Required approvals and failures still surface.

Use an explicit IANA timezone for daily work; otherwise the caller's timezone or
UTC applies. Old bot-owned routines without a saved human owner must be recreated
from an authenticated conversation. Recurring instructions use the agent's
existing tools and access; they do not grant new integrations or credentials.

### Console access and tasks

If the node is reachable on more than loopback, `require_auth` is mandatory: the process refuses to start without it. A binary built without `make web` still starts; the console is simply missing and the log says so.

When compute-teams is off (the default), you get a single-assistant chat with inline approval buttons. Named bot rooms are conversational by default; `/task <instructions>` explicitly starts durable work, and the model can queue work with `task_create` or delegate it. Tasks link to **Task approvals** when an operation or budget needs your decision. If compute is not on this node, set `[ui-web].backend` to a compute node's cluster address. Teams, records, conversations and approvals are served by that backend over cluster mTLS; enable `compute-teams` on the backend to expose its teams without adding local compute to the web node. Login and static assets remain on the web node. A backend outage means unavailable, not deleted history.

In the team console, open **Task approvals** for bot-room, delegated or queued work waiting on you.
The page shows the task's actor, pending operation, state, expiry and budget
consumption. You can approve once, grant an offered operation/category for that
task, deny, or add a bounded budget allowance. Approval queues the saved task for
its worker; **ready** does not mean it has executed. Refresh or use the next-page
button to inspect older tasks. Task approval remains available after closing the
chat that initiated the work.

An **outcome unknown** task may already have produced external effects. Recovery
is offered only when a saved checkpoint is available and requires acknowledging
possible duplicate effects, then a fresh approval. **Close without replay** closes
an uncertain task without running it again; it cannot undo effects already made.
If no checkpoint is available, further work needs a fresh assignment.
Cancelling running work prevents further authorisation but cannot undo effects
already started. Decisions are revision checked; reload a changed task before
deciding again.

Task links open that task directly, even when it is on an older list page.
Completed tasks retain the result plus **Transcript and execution receipts**,
including resumed work. Receipts distinguish actual dispatch from refused,
approval-paused, budget-paused and uncertain attempts. A handler marked executed
may still return a failure. In a bot room, **Conversation and task history** lets
you inspect retained coordinator conversations and task transcripts.

Signing out cancels all active browser streams. Images in model replies appear
as links that you choose to open, rather than loading automatically. Tool
receipts distinguish returned results from attempts that may have been refused,
paused or failed; a returned result alone does not prove the requested external
effect succeeded. Older inbox records show tool attempts because they lack
per-call execution evidence.

### Reviewing learned proposals

Open **Learned proposals** (`/learned`) in either console layout. Its notification
count shows proposals and amendments awaiting your review, including proposals
authored by bots you own. Your account also needs policy permission for
`command:exec` on `learned`, as it does for Telegram's `/learned` command. Being
an operator does not give access to somebody else's proposals.

Choose **Inspect** to see all instructions, current and proposed reference files,
the amendment rationale and source turn. After inspecting the content, confirm
the checkbox and choose **Approve reviewed proposal** or **Reject reviewed
proposal**. An amendment rejection leaves the existing approved version intact.
The receipt reports whether activation actually succeeded, is pending on a
compute node, or failed; recorded approval alone is not proof of activation.

If the revision or content changed during inspection, the decision returns a
conflict. Use **Reload proposal**, inspect the new content and confirm again.
The console never automatically retries a decision or approves newer content
using an older inspection. **Task approvals** remain separate: allowing a task
to continue does not approve a learned skill for activation.

## Telegram

lobslaw supports two transports for Telegram: **poll** (outbound-only long-polling, right for personal deployments) and **webhook** (inbound HTTPS, right for cloud deployments with a stable public URL).

### Setup

1. **Create the bot** with `@BotFather` on Telegram. Message it `/newbot` and follow the prompts. Copy the token it gives you.

2. **Put secrets in `.env`** (the file `lobslaw init` created under `~/.config/lobslaw/`, chmod 0600):

   ```
   TELEGRAM_BOT_TOKEN=8417394926:AAG...
   # Only needed in webhook mode:
   # TELEGRAM_WEBHOOK_SECRET=$(openssl rand -hex 32)
   ```

3. **Add a channel block** to `config.toml`:

   ```toml
   [[gateway.channels]]
   type          = "telegram"
   mode          = "poll"                       # or "webhook"
   bot_token_ref = "env:TELEGRAM_BOT_TOKEN"
   # secret_token_ref = "env:TELEGRAM_WEBHOOK_SECRET"   # webhook mode only

   [gateway.channels.user_scopes]
   "<your-telegram-user-id>" = "owner"
   ```

4. **Restart** the node. You should see `telegram: long-poll loop starting` in the logs (poll mode) or the bot receive your `setWebhook` call (webhook mode).

### Poll vs webhook

| Mode | Needs public HTTPS? | Best for |
|---|---|---|
| `poll` | No | Personal / homelab deployments behind NAT |
| `webhook` | Yes (valid TLS cert) | Cloud-hosted bots with a stable public URL |

Poll mode makes only **outbound** calls to `api.telegram.org` — the bot never accepts inbound connections. Webhook mode is what you'd run behind a load balancer with a registered domain.

If you pick webhook mode, register the webhook once after the node is running:

```bash
curl -X POST "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/setWebhook" \
  -d "url=https://your-public-host/telegram" \
  -d "secret_token=$TELEGRAM_WEBHOOK_SECRET"
```

## User authorization

By default, the gateway rejects Telegram users who aren't explicitly allowed. You authorize users by listing their Telegram **user_id** (not username) in `[gateway.channels.user_scopes]`:

```toml
[gateway.channels.user_scopes]
"6972251926"  = "owner"
"1234567890"  = "family"
```

The value is a lobslaw security scope. `owner` typically has full access; `family` or `public` can be restricted via policy rules. An unknown user is dropped silently unless you set `unknown_user_scope` under `[gateway]`:

```toml
[gateway]
unknown_user_scope = "public"   # or leave empty for strict mode
```

**Pick user_id, not username.** Telegram usernames are mutable — users can change their `@handle` at any time. `user_id` is a stable int64 assigned at account creation and never changes.

### Finding your Telegram user_id

Three easy ways:

1. **Message `@userinfobot`** on Telegram. Send anything, it replies with your user_id (and first_name + username).
2. **Message `@RawDataBot`** — dumps the full update JSON Telegram sends for your message, user_id included.
3. **Telegram Desktop → Settings → Advanced → Export data** — user_id is in the `personal_information.user_id` field of the export.

If you've already configured the bot with your token but haven't added your user_id yet, a less-convenient fourth option: message your bot, then check the lobslaw logs for a line like:

```
WARN telegram: unknown user, UnknownUserScope empty — dropping user_id=6972251926 username=yourhandle
```

### Conversation memory

Each Telegram chat is one durable conversation, automatically — no setup. The transcript is stored in the cluster, so the bot still remembers the thread after a restart, an upgrade, or a failover to another node. Older messages are trimmed once a chat passes `gateway.session_max_messages` (default 200).

Two caveats worth knowing:

- Only the node holding raft leadership can write transcripts. If a message is handled by a follower, the bot stays coherent for that conversation but those turns won't survive a restart. Single-node deployments never hit this.
- A chat that outgrows the model's context window will still fail; the transcript is kept, but nothing summarises it down yet.

## What Telegram gives us (and doesn't)

Each inbound message carries:

| Field | Stable? | Notes |
|---|---|---|
| `user_id` | **Yes** | int64, assigned at account creation, never changes |
| `username` | No | User can change or remove their `@handle` at any time |
| `first_name`, `last_name` | No | Display only, user-mutable |
| `language_code` | Per-message | BCP 47 locale hint from the user's client |
| `is_bot`, `is_premium` | Sometimes | Flags |

Telegram's Bot API does **not** give us:

- **Phone number** — only when the user explicitly taps a `requestContact` keyboard button and shares it. Not available on normal messages.
- **Email address** — never.

That's why `user_scopes` keys on `user_id`: it's the only identifier that's both stable and present on every message.


REST images and audio use a two-step flow: send raw bytes to `/v1/uploads`
with a supported media `Content-Type`, then include `upload_ids: ["upload-…"]`
in `/v1/messages`. Both requests require a valid JWT for the same user and must
reach the same gateway process. Upload IDs expire after one hour and do not
survive restart. See [upload examples and limits](../docs/configuration/channels.md).
