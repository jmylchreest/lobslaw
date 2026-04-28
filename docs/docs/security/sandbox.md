---
sidebar_position: 4
---

# Sandbox

How `internal/sandbox` isolates tool subprocesses from the host.

## TL;DR

A sandboxed tool runs with **layered defences**, each a separate kernel mechanism:

1. **User + PID + mount + (optionally) network namespace** via `syscall.SysProcAttr.Cloneflags`.
2. **Path canonicalisation + containment** (`CanonicalizeAndContain`) — no symlink-out, no `..`, no hardlink escape.
3. **Policy validation** — `Policy.Validate` rejects relative paths, read-only-outside-allowed, negative quotas.
4. **`NoNewPrivs` + Landlock LSM + seccomp BPF deny-list** — installed via the `lobslaw sandbox-exec` reexec helper.
5. **Optional nftables egress redirect** — netns rules redirecting traffic to the smokescreen UDS.
6. **cgroup v2 CPU/memory limits** — deferred. `WaitDelay = 500ms` already bounds wall-clock.

Every layer composes; none replaces another.

## Threat target

A compromised or malicious tool subprocess, invoked by the agent, with access to only what the operator explicitly allowed. This is **defence-in-depth against tool misbehaviour**, not a sovereign security boundary.

Out of scope: kernel exploits, hardware side-channels, the agent itself being compromised.

## What stops what

| Attack | Defeated by |
|---|---|
| Tool reads `/etc/passwd` it was never allowed | Landlock + mount namespace |
| Tool writes outside `allowed_paths` | Landlock |
| Tool `mount`s tmpfs over `/tmp` to spoof caches | seccomp (`mount`) + mount namespace |
| Tool calls `ptrace` on the agent | seccomp (`ptrace`) + PID namespace |
| Tool `execve`s a setuid binary to escalate | NoNewPrivs |
| Tool loads a kernel module | seccomp (`init_module`) |
| Tool exfiltrates via `bpf()` / `keyctl()` | seccomp (`bpf`, `keyctl`) |
| Symlink traversal to `/etc/passwd` | `CanonicalizeAndContain` |
| Allowed file hardlinked to the agent's DB | `RequireSingleLink` (st_nlink check; opt-in) |
| Tool stalls forever keeping stdio open | `WaitDelay = 500ms` |
| Tool exfiltrates via DNS to attacker.com | egress ACL on smokescreen + (optional) nftables |

## What the sandbox doesn't cover

### Prompt injection

The sandbox controls what a tool **can do once it's running**. It does not judge whether the tool should have been invoked, or whether the arguments make sense for the user's actual intent.

Prompt-injection defence lives in three other places:

| Mechanism | File | Purpose |
|---|---|---|
| Policy engine | `internal/policy/engine.go` | `tool:exec` rules — deny or `require_confirmation` |
| PreToolUse hooks | `internal/hooks/dispatcher.go` | Arbitrary code blocking based on turn intent |
| Registry constraints | `internal/compute/registry.go` | `allowed_paths`, argv templates, param shapes |

For tools with broad capabilities (anything that writes, executes, or sends data off the host), `require_confirmation` is the correct defence — not tighter sandboxing.

## Architecture: why a reexec helper?

Three of the enforcement steps (**NoNewPrivs, Landlock, seccomp**) must be installed in the target process, **between `fork()` and `execve()`**. Applying them to the Go parent poisons the parent. Go's stdlib has no post-fork-pre-exec hook beyond `SysProcAttr` fields, which don't include Landlock or seccomp.

The standard solution (used by runc, containerd, podman) is the **reexec helper pattern**:

```
Parent (lobslaw agent)
  │  sandbox.Apply rewrites cmd:
  │    cmd.Path = /proc/self/exe
  │    argv     = ["lobslaw", "sandbox-exec", "--", "/real/tool", ...]
  │    env     += LOBSLAW_SANDBOX_POLICY=<base64>
  │
  └──► clone() with CLONE_NEWUSER|NEWNS|NEWPID|...
         │
         └──► execve(/proc/self/exe sandbox-exec ...)
                │
                ├── prctl(PR_SET_NO_NEW_PRIVS, 1)
                ├── landlock_create_ruleset + add_rule + restrict_self
                ├── seccomp(SET_MODE_FILTER, TSYNC, &bpf)
                └──► execve(/real/tool, args, env)
                        │
                        └── Tool runs with all enforcement active
```

`lobslaw sandbox-exec` is a hidden subcommand of the main binary. `sandbox.Apply` rewrites any `exec.Cmd` whose Policy carries an enforcement field to invoke `/proc/self/exe sandbox-exec --` followed by the original target + argv. The child reads the serialised policy from `$LOBSLAW_SANDBOX_POLICY`, performs the three installs, unsets the env var, then `execve`s the actual tool.

**Namespaces stay separate** — `Apply` applies `CLONE_NEW*` via `SysProcAttr.Cloneflags` directly. Policies that only ask for namespaces don't pay reexec cost.

This technique is what the active [Go proposal #68595](https://github.com/golang/go/issues/68595) replicates. Until that lands, we do it ourselves.

## Library choices

| Layer | Library | Why |
|---|---|---|
| Landlock ruleset build | `github.com/landlock-lsm/go-landlock` | Pure Go, ergonomic, maintained by Landlock authors |
| Seccomp BPF filter | `github.com/elastic/go-seccomp-bpf` | Pure Go, production-used, CGO=0 |
| NoNewPrivs install | `golang.org/x/sys/unix.Prctl` | Stdlib-adjacent |

CGO is disabled for the lobslaw binary; all sandbox primitives are pure Go.

## nftables egress

Optional layer, added when a skill declares `network_isolation: true` (own netns) and the operator wants traffic *redirected* rather than *denied*.

Without nftables: a netns-isolated subprocess has no network. With nftables: rules in the new netns redirect outbound connections to the smokescreen UDS (under `[security] egress_uds_path`). The subprocess's egress is still ACL'd by smokescreen role.

The smokescreen process itself is excluded from the redirect (avoiding loops). Rule generation lives in `internal/sandbox/netfilter/rules.go`; apply is `internal/sandbox/netfilter/apply_linux.go` (no-op on other OSes).

## Reference

- `internal/sandbox/policy.go` — policy struct + validation
- `internal/sandbox/install_linux.go` — apply on Linux
- `internal/sandbox/install_other.go` — chroot+cwd fallback for macOS/BSD
- `internal/sandbox/sandbox-exec.go` — reexec helper subcommand
- `internal/sandbox/netfilter/` — nftables generator + apply
- `cmd/lobslaw/sandbox-exec.go` — hidden CLI entry
