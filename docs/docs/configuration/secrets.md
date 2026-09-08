---
title: Secrets
description: Resolve keys and tokens from Bitwarden, 1Password, or any local vault instead of the node's own disk.
---

# Secrets

Every `_ref` field in lobslaw takes a **secret reference**, never a secret. A literal is refused outright, so a plaintext key cannot be committed in a config file.

Two references always work and resolve against this machine:

| Reference              | Resolves to                                    |
| ---------------------- | ---------------------------------------------- |
| `env:VAR_NAME`         | that environment variable                      |
| `file:/path/to/secret` | the file's contents, trailing newline stripped |

Anything else is a **provider label** you declare, which is how a key lives in a vault instead of on every node's disk.

## Declaring a provider

A provider's `label` _is_ the reference scheme it answers to. Declare one and `bw:app/key` works anywhere `env:APP_KEY` works today:

```toml
[[secrets.providers]]
label      = "bw"
driver     = "bitwarden"
env        = { BW_CONFIG_DIR = "/etc/lobslaw/bw" }   # plaintext
secret_env = { BW_SESSION = "env:BW_SESSION" }       # secret references

[[compute.providers]]
label       = "openrouter"
api_key_ref = "bw:lobslaw/openrouter"     # ← the vault, not the disk
```

Three drivers ship:

| `driver`      | Backend      | Notes                                                                    |
| ------------- | ------------ | ------------------------------------------------------------------------ |
| `bitwarden`   | the `bw` CLI | defaults to the item's `password` field; `options.field` selects another |
| `onepassword` | the `op` CLI | path is `vault/item/field` — the `op://` URI without its scheme          |
| `exec`        | any command  | the long tail: `pass`, `gopass`, `sops`, `age`, `systemd-creds`          |

### Any local vault

`exec` runs a configured argv and takes its stdout, so a tool nobody has written a driver for needs no Go:

```toml
[[secrets.providers]]
label   = "pass"
driver  = "exec"
command = ["pass", "show", "{{path}}"]

[[secrets.providers]]
label   = "sops"
driver  = "exec"
command = ["sops", "--decrypt", "--extract", "{{path}}", "secrets.enc.yaml"]
```

`{{path}}` is replaced with whatever follows the scheme in the reference. With no placeholder anywhere in the argv, the path is appended as a final argument — which is what `pass show <path>` wants anyway.

It is an **argv, never a shell string**. A secret path containing a space must not be able to become a second command.

| Option            | Default | Meaning                                                         |
| ----------------- | ------- | --------------------------------------------------------------- |
| `trim_whitespace` | `true`  | strip surrounding whitespace from stdout                        |
| `env_passthrough` | none    | comma-separated names of extra environment variables to inherit |

Both are understood by every command-backed driver, including `bitwarden` and `onepassword`.

`trim_whitespace` defaults to on because a CLI that prints a trailing newline is the norm, and a key with `\n` on the end fails authentication in a way nothing reports usefully. Turn it off for the rare secret whose newline is load-bearing.

`env_passthrough` takes **names, never patterns**. See [what the subprocess can see](#what-the-subprocess-can-see) below.

### Environment for the subprocess

`env` values are **plaintext**; `secret_env` values are **secret references**, resolved through the bootstrap schemes only.

The split matters because most of what a vault CLI needs in its environment is not secret — a config directory, a CA bundle path, an account alias — and it exists in exactly the shape `[mcp.servers.<name>]` already uses. If both name the same variable, `secret_env` wins: that is the only ordering that cannot silently downgrade a secret to a literal.

`env` used to be the reference field. A config carrying `env = { BW_SESSION = "env:BW_SESSION" }` is refused at boot with an error naming `secret_env`, rather than quietly handing the CLI the literal string `"env:BW_SESSION"` as its token.

TOML inline tables must fit on one line, so use a sub-table when the values are long:

```toml
[[secrets.providers]]
label  = "bw"
driver = "bitwarden"

[secrets.providers.env]
BITWARDENCLI_APPDATA_DIR = "/var/lib/lobslaw/bw"
NODE_EXTRA_CA_CERTS      = "/etc/ssl/certs/internal-ca.pem"

[secrets.providers.secret_env]
BW_SESSION = "file:/run/secrets/bw-session"
```

### What the subprocess can see

A vault subprocess gets an **allowlisted** slice of the node's environment, plus everything the provider declared. It does not inherit the node's environment wholesale.

It used to. That meant fetching one secret exposed every other one: a node holding the provider API keys, the channel tokens and the memory encryption passphrase in its environment passed all of them to `pass`, to `bw`, and to whatever argv was configured. The command comes from `config.toml` rather than from an attacker, so this was a blast-radius problem rather than a way in, but any one vault CLI, wrapper script, or mistaken argv saw the lot.

Three routes in, and only three:

1. **The base allowlist**, what a vault CLI needs to run at all. `HOME`, `PATH`, `USER`, `LOGNAME`, `TMPDIR`; the locale variables; the `XDG_*` base directories; `GNUPGHOME` and `GPG_TTY`; and the socket and display addresses a pinentry or an agent is reached through (`SSH_AUTH_SOCK`, `DBUS_SESSION_BUS_ADDRESS`, `DISPLAY`, `WAYLAND_DISPLAY`).

   It holds no secret _values_, which is not the same as holding nothing sensitive. `SSH_AUTH_SOCK` and `DBUS_SESSION_BUS_ADDRESS` are capability handles: the value is a socket path, but a process that can reach the agent can ask it to sign, and one that can reach the session bus can ask the keyring for items. They are allowed because a vault that cannot reach its agent or its keyring cannot answer. If you need a subprocess held tighter than that, give it its own dedicated agent socket rather than the operator's.

2. **The driver's own sign-in variables.** Each compiled vendor driver adds the ones its CLI authenticates with, so the workflows its own error messages recommend keep working:

   | Driver        | Allowed                                                                                                                                |
   | ------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
   | `bitwarden`   | `BW_SESSION`, `BW_CLIENTID`, `BW_CLIENTSECRET`, `BITWARDENCLI_APPDATA_DIR`                                                             |
   | `onepassword` | `OP_SERVICE_ACCOUNT_TOKEN`, `OP_ACCOUNT`, `OP_CONFIG_DIR`, `OP_CONNECT_HOST`, `OP_CONNECT_TOKEN`, `OP_SESSION`, and any `OP_SESSION_*` |

   Both sign-in routes work, not just the non-interactive one. `eval $(op signin <shorthand>)` exports `OP_SESSION_<shorthand>`, whose name is not fixed, which is why that one is a prefix.

3. **`env` and `secret_env`**, declared and so never filtered. Declaring a variable _is_ the authorisation, and a declared value wins over an inherited one of the same name.

### Choosing between them

In order of preference:

- **An ordinary setting** (a config directory, a CA bundle path, an account alias) goes in **`env`**. It is plaintext, visible in the config, and the same shape `[mcp.servers.<name>]` already uses.
- **A credential** (a session token, a service account token, an API key) goes in **`secret_env`**, resolved through the bootstrap schemes. Never put one in `env`, where it is a literal in the config file.
- **`env_passthrough`** is the last resort, for a variable the allowlist does not know about that you cannot or will not declare:

  ```toml
  [[secrets.providers]]
  label   = "pass"
  driver  = "exec"
  command = ["pass", "show", "{{path}}"]
  options = { env_passthrough = "PASSWORD_STORE_DIR,PASSWORD_STORE_GPG_OPTS" }
  ```

  It works on every command-backed driver, `bitwarden` and `onepassword` included. Exact names only, comma separated. A value containing `*`, `?` or `[` is **refused at boot** with an error naming the setting, rather than matched literally: a glob there would reinstate wholesale inheritance in a setting that reads as though it is being careful.

  Prefer `env` or `secret_env` where you can. A declared value is visible in the config; a passthrough depends on whatever happened to be in the node's environment at boot, which is the class of thing that works on one host and not the next.

### Migrating an existing provider

If a provider stops finding a variable after this change, it was relying on wholesale inheritance. Work down the list:

1. Is it a **credential**? Move it to `secret_env`. This is the right answer for `BW_SESSION`, `OP_SERVICE_ACCOUNT_TOKEN` and anything like them if you are not exporting them into the node's environment already.
2. Is it an **ordinary setting**? Move it to `env`.
3. Is it neither, or set by a tool you do not control? Name it in `env_passthrough`.

`lobslaw doctor` resolves one real reference through every provider, so it will tell you which provider is unhappy before a turn does.

## The bootstrap floor

Three things **must** use `env:` or `file:`, and are refused otherwise with an error saying so:

- `memory.encryption.key_ref`
- the `[cluster.mtls]` paths
- any `[[secrets.providers]]` credential

This is not a policy choice, it is the order things happen in. The memory key is resolved before the node is constructed — before any wiring stage exists, including the one that would build a provider. And a vault whose own credential came from another vault needs one of them working before either does.

The error names the constraint rather than reporting an unknown scheme, because _"unknown scheme: bw"_ is a confusing thing to read when `bw` is configured and working three lines further down the same file.

## Checking it works

`lobslaw doctor` builds every declared provider **and resolves one real reference through each**:

```
OK    secret providers: bw ✓, pass ✓
```

Constructing a provider is not enough to know it works — a missing binary, a locked vault and an expired session all construct fine and fail at the first fetch, which on this node happens during boot. The probe uses a reference your config actually contains rather than one invented for the check.

## Caching

A resolved value is reused for `secrets.cache_ttl` (default 5 minutes). One boot resolves the same reference several times — the chat driver, the capability probe and doctor all read the same provider key — and on a CLI-backed vault each of those would otherwise be a separate process.

## What this is not

`[[secrets.providers]]` is about where **lobslaw's own configuration** gets its secrets.

It is unrelated to the OAuth credential store (`internal/memory/credentials.go`), which holds credentials **the agent's skills use on your behalf** — encrypted with the cluster key, never decrypted into the agent's context, gated per skill. The two are deliberately separate and a vault provider is not a second route into that bucket.

## Known limitation

Secret-provider subprocesses do **not** route through the smokescreen egress proxy. A `bw` or `op` CLI reaching its cloud egresses directly.

The reason is the bootstrap floor above: some resolution happens before the egress stage exists, so a proxy cannot be applied uniformly — and applying it to some resolutions and not others would be worse than applying it to none. Tracked in `DEFERRED.md`.

## Rotation

Not yet. Secrets resolve at boot into constructed drivers, so rotating one in the vault takes a restart — which is what it took before providers existed too.
