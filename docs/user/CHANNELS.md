# Channels

lobslaw exposes the agent loop to users through **channels**. Today there are two: the REST API and Telegram. Channels are configured under `[[gateway.channels]]` in `config.toml`; you can mix and match.

## REST

Default. Mounts on the gateway HTTP port (8443 by default) at:

- `POST /v1/messages` — send a message, get a reply
- `GET /v1/plan` — see what's scheduled and in-flight
- `POST /v1/prompts/{id}/{approve|deny}` — answer a confirmation prompt
- `GET /healthz`, `GET /readyz` — health probes

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
