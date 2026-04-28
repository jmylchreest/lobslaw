---
sidebar_position: 4
---

# First Message

End-to-end smoke test. After this you'll have a running cluster, a bound user, and an installed skill that talks to a real third-party API.

## 1. Hello

In Telegram, send your bot:

> hi

The bot should reply within a few seconds. If it doesn't, check the logs.

## 2. Bind yourself as the operator

The first message from a new Telegram user creates an unbound session. To unlock the operator-scoped tools (everything sensitive — credentials, OAuth, clawhub install, soul tuning), bind your user:

> bind me as owner

Behind the scenes the agent calls a built-in that writes a user prefs entry mapping your Telegram chat ID → user ID `owner`, with the `scope:owner` claim attached. From now on, your messages on this chat get the operator scope.

## 3. Hook up a Google credential

If you set `[security.oauth.google]` in `config.toml`:

> start oauth flow for google

The bot replies:

```
Started flow 01HXAB.... Visit https://www.google.com/device and enter code ABCD-EFGH.
```

Open the URL on a phone or laptop, enter the code, approve. The bot's background poller catches the grant within ~30s and persists an encrypted credential keyed by your Google subject.

> oauth status

```
1 flow complete (google, alice@example.com, scopes: openid email profile)
```

## 4. Install a skill

```bash
lobslaw plugin install clawhub:gws-workspace@1.0.0
```

(Or via the agent if you've added a `clawhub_install` policy allow rule:
> install the gws-workspace skill from clawhub)

The bundle is fetched from `clawhub.ai`, signature-verified per `[security] clawhub_signing_policy`, extracted into the install mount, and registered with the tool registry.

## 5. Grant the skill access

The skill is installed but has no credentials yet. Tell the agent:

> grant gws-workspace access to my google credential, scopes gmail.readonly and calendar.readonly

The bot creates an ACL entry (encrypted-at-rest, raft-replicated) authorizing the skill's invoker to obtain a fresh access token for those scopes whenever it spawns.

## 6. Use it

> what's on my calendar tomorrow?

The agent decides to call `gws-workspace.calendar.list_events`. The skill subprocess spawns inside its sandbox, the invoker pre-fetches a fresh access token (refreshing if needed), env-injects it as `LOBSLAW_CRED_GOOGLE_TOKEN`, the skill reads it, queries Google Calendar through the egress proxy on the `skill/gws-workspace` role, and returns JSON. The agent narrates.

## What just happened

You walked the full trust boundary:

```
   user
    │   (Telegram, mTLS-terminated at gateway)
    ▼
  gateway ─► policy engine ─► agent
                                 │
                                 ▼
                              tool dispatch
                                 │
                ┌────────────────┼─────────────────┐
                ▼                ▼                 ▼
            builtin           skill             MCP server
            (in-proc)         (sandboxed)       (subprocess)
                                 │
                                 ▼
                          smokescreen ACL
                                 │
                                 ▼
                              internet
```

If any layer says no, the call is denied — and the agent surfaces *why*.

## Where to go next

- [Security model](/security/threat-model) — what each layer protects against
- [Configuration reference](/configuration/reference) — every TOML knob
- [Skills](/features/skills) — write your own
- [Commitments](/features/commitments) — the agent promising to do things later
