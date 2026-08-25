# lobslaw-local — one container, one directory

A single lobslaw node under rootless podman, with every byte it
persists living in one directory on the host. Wiping it is `rm -rf`;
backing it up is `tar`; editing its config is opening a file.

This is the local-scratch counterpart to `deploy/docker/`, which runs a
multi-service compose stack with init containers and named volumes.
Here there are no sidecars, no named volumes, and no compose file.

```bash
cd deploy/podman
./lobslaw-local build
./lobslaw-local up
./lobslaw-local status
```

`up` bootstraps on first run — CA, node certificate, config, soul,
memory key, embedding model — and is a no-op on every run after that.

## Where things live

Default state directory is `~/.local/share/lobslaw-local`; override with
`LOBSLAW_STATE_DIR`. `./lobslaw-local path` prints it.

| Host (under `<state>/`) | Container            | What                                     |
| ----------------------- | -------------------- | ---------------------------------------- |
| `config/config.toml`    | `/etc/lobslaw`       | the node's config — edit it here         |
| `config/SOUL.md`        | `/etc/lobslaw`       | the soul — edit it here                  |
| `config/certs/`         | `/etc/lobslaw/certs` | cluster CA + this node's mTLS cert       |
| `data/`                 | `/var/lobslaw/data`  | `raft.db`, `state.db`, snapshots, models |
| `audit/`                | `/var/lobslaw/audit` | `audit.jsonl`                            |
| `skills/`               | `/var/lobslaw/skills`| clawhub bundles                          |
| `workspace/`            | `/workspace`         | agent files, inbound attachments         |
| `usr-local/`            | `/lobslaw/usr/local` | `binary_install` prefix                  |
| `.env`                  | *(environment only)* | memory key + provider API keys           |

`.env` is the exception: it is read on the host and injected with
`--env-file`, so the provider keys exist in the container's environment
but never in its filesystem. `lobslaw doctor` reports
`FAIL .env readable + chmod 0600` because of this — it looks for the
file next to `config.toml`, and here there deliberately isn't one. Every
other doctor check should pass.

### Ownership

Rootless podman runs the container with
`--userns=keep-id:uid=65532,gid=65532`, mapping your host user onto the
image's `nonroot` uid. Files the agent creates in `workspace/` are owned
by *you* on the host — no `sudo` to read them, no root-owned droppings
in `$HOME`. Under rootful podman the script chowns the state directory
to `65532` instead.

## First run

`up` needs no arguments, but the node has no LLM key until you give it
one:

```bash
$EDITOR "$(./lobslaw-local path)/.env"     # LOBSLAW_MINIMAX_API_KEY=...
./lobslaw-local restart
```

Memory, policy, storage, scheduler, audit and the REST gateway all work
without a provider key. Only the agent loop needs one.

Then:

```bash
./lobslaw-local ask "what tools do you have?"
curl -s localhost:8443/healthz
```

## Day to day

```bash
./lobslaw-local logs                  # follow (pass --tail=50 etc.)
./lobslaw-local shell                 # bash in the running container
./lobslaw-local exec doctor --config /etc/lobslaw/config.toml
./lobslaw-local exec memory list      # any lobslaw subcommand
./lobslaw-local restart               # after editing config.toml
```

`exec` runs a one-shot container over the same state with the same uid
mapping, so what it writes is indistinguishable from what the node
writes. The running node holds an exclusive lock on `raft.db`, so
anything that opens raft wants the node stopped first.

Most of `config.toml` is picked up live — `[config] watch = true` — but
restart if a change looks like it hasn't landed.

## Resetting

```bash
./lobslaw-local reset   # wipe raft state; keep config, certs, memory, workspace
./lobslaw-local nuke    # delete the whole state directory (asks first)
```

`reset` is what you want after changing `LOBSLAW_NODE`, which changes
the raft server ID and leaves the old one orphaned in the persisted
configuration. Note that renaming the node also invalidates the mTLS
cert, whose CN is bound to it — delete `config/certs/node-*.pem` and
re-run `up` to reissue.

## Running at login

```bash
./lobslaw-local quadlet
systemctl --user daemon-reload
systemctl --user enable --now lobslaw-local
```

That writes a Quadlet unit to
`~/.config/containers/systemd/lobslaw-local.container` describing the
same container the script starts. `loginctl enable-linger $USER` if you
want it up without being logged in.

## Configuration worth knowing about

**Discovery is off.** `broadcast = false` in `config.toml`. Leave it
that way: with broadcast on, this container would hear — and try to
join — any other lobslaw node on the LAN, including a test stack. A
local scratch node should stay a cluster of one.

**The gateway is plaintext HTTP on loopback.** No TLS cert is
configured and the port is published on `127.0.0.1`. Don't move it off
loopback without putting TLS in front.

**Embeddings run in-process.** `type = "builtin"`, `all-MiniLM-L6-v2`,
384 dims — no API key, no endpoint, and no egress, so memory content
never leaves the machine. The ~90MB checkpoint is fetched **on the
host** by `bootstrap` into `data/models/`, and `download_url` is left
empty so the node fetches nothing at runtime.

Setting `download_url` and letting the node fetch it also works — the
`embedding-model` egress role covers the CDN hosts a HuggingFace repo
redirects weights to. Doing it on the host is the better default here
anyway: boot stays offline, and the checkpoint is a visible file in the
state directory rather than something that appears inside a volume.

Set `LOBSLAW_EMBED_MODEL=""` to skip the model entirely — bootstrap then
drops the `[compute.embeddings]` block and recall falls back to lexical
matching.

**Skills and MCP tools are policy-denied by default.** Builtins get a
default-allow seed; everything else needs an explicit rule. There are
commented examples at the bottom of the policy section in
`config.toml`.

## Environment overrides

| Variable                  | Default                        |
| ------------------------- | ------------------------------ |
| `LOBSLAW_STATE_DIR`       | `~/.local/share/lobslaw-local` |
| `LOBSLAW_IMAGE`           | `lobslaw:local`                |
| `LOBSLAW_CONTAINER`       | `lobslaw-local`                |
| `LOBSLAW_NODE`            | `lobslaw-local`                |
| `LOBSLAW_BIND`            | `127.0.0.1`                    |
| `LOBSLAW_GATEWAY_PORT`    | `8443`                         |
| `LOBSLAW_CLUSTER_PORT`    | `7443`                         |
| `LOBSLAW_NETWORK`         | *(empty — publish ports)*      |
| `LOBSLAW_MOUNT_OPTS`      | *(empty; use `,Z` on SELinux)* |
| `LOBSLAW_LOG_LEVEL`       | `info`                         |
| `LOBSLAW_LOG_FORMAT`      | `text`                         |
| `LOBSLAW_EMBED_MODEL`     | `all-MiniLM-L6-v2`             |
| `LOBSLAW_CONTAINERFILE`   | `../../Dockerfile`             |

Two stacks side by side want a different `LOBSLAW_STATE_DIR`,
`LOBSLAW_CONTAINER`, `LOBSLAW_NODE` and ports.

`LOBSLAW_NETWORK=host` puts the container on the host network namespace
instead of publishing ports. Useful if you want the node reachable at
the host's own address, and the only thing that works on a host whose
kernel cannot open `/dev/net/tun`.

## When the build fails

`podman build` mounts an overlay over the build context. If the running
kernel has no overlayfs — most often a machine that has had a kernel
package upgraded but not yet rebooted, so `/lib/modules/$(uname -r)` no
longer exists — every `podman build` fails with:

```
mounting an overlay over build context directory: ... no such device
```

`./lobslaw-local build` detects this, says so, and falls back to
building the binary with the host Go toolchain and assembling the image
with `podman run` + `podman commit`, which only needs the storage
driver. The result is the same image. Rebooting into the installed
kernel restores the normal path.
