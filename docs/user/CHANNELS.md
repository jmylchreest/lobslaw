# Channels

lobslaw exposes the agent loop to users through **channels**. Today there are REST, an optional browser console, and Telegram. Channels are configured under `[[gateway.channels]]` in `config.toml`; you can mix and match.

## REST

Default. Mounts on the gateway HTTP port (8443 by default) at:

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

Then open the gateway HTTP port in a browser (8443 by default). On the same computer, click **Continue on this computer**. From a phone, run `lobslaw login --config <path>` on the node and type the six-digit code. There is no password and no self-signup: the person must already be in `[[user]]`.

If the node is reachable on more than loopback, `require_auth` is mandatory: the process refuses to start without it. A binary built without `make web` still starts; the console is simply missing and the log says so.

When compute-teams is off (the default), you get a single-assistant chat. The console will not invent a team. If compute is not on this node, set `[ui-web].backend` to a compute node's cluster address; when that backend is unreachable, chat shows unavailable rather than an empty history.

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
