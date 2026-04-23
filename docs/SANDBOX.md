# Sandbox

How `internal/sandbox` isolates tool subprocesses from the host, and why the
architecture looks the way it does.

## TL;DR

A sandboxed tool runs with **layered defences**, each a separate kernel mechanism:

1. **User + PID + mount + (optionally) network namespace** — cheap isolation via `syscall.SysProcAttr.Cloneflags`. Wired.
2. **Path canonicalisation + containment** (`CanonicalizeAndContain`) — no symlink-out, no `..` traversal, no hardlink escape (`RequireSingleLink`). Wired.
3. **Policy validation** — `Policy.Validate` rejects relative paths, read-only-outside-allowed, negative quotas. Wired.
4. **`NoNewPrivs` + Landlock filesystem LSM + seccomp BPF deny-list** — layered enforcement via the `lobslaw sandbox-exec` reexec helper. **Wired (Phase 4.5.5).**
5. **cgroup v2 CPU/memory limits** — deferred; `WaitDelay` already bounds wall-clock.
6. **nftables egress control** — deferred; `CLONE_NEWNET` already isolates network by default.

Every layer composes with every other; none replaces any of the others.

---

## Threat model

Target: **a compromised or malicious tool subprocess, invoked by the agent, with access to only what the operator explicitly allowed.**

The sandbox is a **capability boundary**: it controls *what a running tool can touch*. It is not the layer that decides *whether the tool should have been invoked in the first place* — that's upstream, and is covered below under "What the sandbox doesn't cover".

Not in scope: kernel exploits, hardware side-channels, or the agent itself being compromised. This is a **defence-in-depth against tool misbehaviour**, not a sovereign security boundary.

Concrete attacks we want to stop, and which layer stops them:

| Attack | Defeated by |
|---|---|
| Tool reads `/etc/passwd` it was never allowed | Landlock + mount namespace |
| Tool writes outside `allowed_paths` | Landlock |
| Tool `mount`s a tmpfs over `/tmp` to spoof caches | Seccomp (`mount` in deny-list) + mount namespace |
| Tool calls `ptrace` on the agent | Seccomp (`ptrace` in deny-list) + PID namespace |
| Tool `execve`'s a setuid binary to escalate | NoNewPrivs |
| Tool loads a kernel module | Seccomp (`init_module` in deny-list) |
| Tool exfiltrates via `bpf()` / `keyctl()` | Seccomp (`bpf`, `keyctl` in deny-list) |
| Path that resolves to `/etc/passwd` via symlink | `CanonicalizeAndContain` (EvalSymlinks + containment) |
| Allowed file hardlinked to the agent's DB | `RequireSingleLink` (st\_nlink check; opt-in) |
| Tool stalls forever keeping stdio open | `WaitDelay = 500ms` on `exec.Cmd` |

---

## What the sandbox doesn't cover

### Prompt injection / tool-call legitimacy

The sandbox controls what a tool **can do once it's running**. It does not judge whether the tool should have been invoked, or whether the arguments make sense for the user's actual intent. That's a **semantic** problem — the LLM read some untrusted content (a file, a web page, a message) and was steered into issuing a tool call the user didn't authorise.

Prompt injection defence lives in three other places in the stack, and they matter more than the sandbox for this class of attack:

| Mechanism | File / package | What it does |
|---|---|---|
| Policy engine | `internal/policy/engine.go` | Rules keyed on `action` + `resource` + claims. `tool:exec` on a risky tool can be denied or `require_confirmation` outright, at the gate. |
| PreToolUse hooks | `internal/hooks/dispatcher.go` | Allow a hook to block a tool call based on arbitrary code — "is this invocation consistent with the current turn's intent?". |
| Registry constraints | `internal/compute/registry.go` | `allowed_paths`, argv templates, and param shapes constrain what a tool can even be **asked** to do. A `read_file` tool restricted to `/home/user/projects` gives injection a much smaller address space to target. |

In the security model, the sandbox is the **last line**: if policy, hooks, and registry constraints all let a bad invocation through, the sandbox ensures the *blast radius* is bounded by the tool's declared capabilities. It does not — and cannot — distinguish a legitimate `read_file /tmp/notes.md` from an injected one.

**Practical implication:** for tools with broad capabilities (anything that writes, executes, or sends data off the host), the right defence is `require_confirmation` in policy, not tighter sandboxing. A human-in-the-loop prompt is the only robust counter to "the LLM was tricked into doing something".

---

## Architecture: why a reexec helper?

Three of the above enforcement steps (**NoNewPrivs, Landlock, seccomp**) have the same constraint: **they must be installed in the target process, between `fork()` and `execve()`.** Applying them to the Go parent poisons the parent (the agent can no longer do anything the sandbox disallowed). Go's stdlib exposes no post-fork, pre-exec hook other than the limited set of `SysProcAttr` fields, and those don't include Landlock or seccomp.

The standard solution, used by runc / containerd / podman, is the **reexec helper pattern**:

```
lobslaw (agent)
 └── fork + clone namespaces (via SysProcAttr.Cloneflags)
      └── /proc/self/exe sandbox-exec -- /real/tool [args…]
           ├── LOBSLAW_SANDBOX_POLICY env → base64(JSON) Policy
           ├── prctl(PR_SET_NO_NEW_PRIVS, 1)
           ├── landlock_create_ruleset + add_rule + restrict_self
           ├── seccomp(SECCOMP_SET_MODE_FILTER, …) with TSYNC
           └── execve(/real/tool, [args…])
```

`lobslaw sandbox-exec` is a hidden subcommand of the main binary. `sandbox.Apply` rewrites any `exec.Cmd` whose Policy carries an enforcement field (NoNewPrivs, AllowedPaths, or Seccomp.Deny) to invoke `/proc/self/exe sandbox-exec --` followed by the original target path + argv. The child reads the serialised policy from `$LOBSLAW_SANDBOX_POLICY`, performs the three installs, unsets the env var so the target doesn't inherit it, then `execve`'s the actual tool. No separate binary to ship.

**Namespaces stay separate:** `Apply` applies CLONE_NEW* via `SysProcAttr.Cloneflags` directly — the helper isn't involved. Policies that only ask for namespaces (e.g. for lightweight tests) don't pay reexec cost. The helper rewrite is gated by `needsReexec` — effectively "does the policy have an enforcement field set".

This exact technique is **also what the active Go proposal ([golang/go#68595](https://github.com/golang/go/issues/68595)) replicates** — it adds post-fork-pre-exec landlock + NoNewPrivs install directly to the runtime's `forkExec` path, so callers don't need a helper. Until that lands, we do it ourselves.

---

## Library choices

| Layer | Library | Why |
|---|---|---|
| Landlock ruleset build | `github.com/landlock-lsm/go-landlock` | Pure Go, ergonomic API, maintained by landlock authors |
| Seccomp BPF filter | `github.com/elastic/go-seccomp-bpf` | Pure Go, production-used by Elastic Beats, CGO=0 |
| NoNewPrivs install | `golang.org/x/sys/unix.Prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)` | Stdlib-adjacent; no dep |

All three compose: NoNewPrivs is a prerequisite for Landlock (kernel enforces this); Landlock + seccomp are independent.

Related decisions:
- `lobslaw-seccomp-library` → `elastic/go-seccomp-bpf` (2026-04-22)
- `lobslaw-filesystem-sandbox` → `landlock-lsm/go-landlock` (2026-04-22)
- `go-cgo` project policy → `CGO_ENABLED=0` unless there's no alternative

---

## Upstream tracking

We are actively watching two Go proposals and one CL. If either of the first two lands, we can migrate the corresponding layer out of our helper and into `SysProcAttr` fields with no caller-visible change.

### [#68595](https://github.com/golang/go/issues/68595) — `proposal: syscall: support process sandboxing using Landlock on Linux`

**State:** Open. In proposal review. Active as of 2026-04-16.

**Author:** Günther Noack (`gnoack`), a Landlock kernel maintainer.

**CL:** [go.dev/cl/745940](https://go-review.googlesource.com/c/go/+/745940) — ~40-line patch to `src/syscall/exec_linux.go`.

**Proposed API:**

```go
cmd.SysProcAttr = &syscall.SysProcAttr{
    UseLandlockRestrictSelf:       true,
    LandlockRestrictSelfRulesetFD: rulesetFD,
    LandlockRestrictSelfFlags:     flags,
}
```

**Payload:** Between `fork()` and `execve()`, the Go runtime will call:

```c
prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0);
landlock_restrict_self(ruleset_fd, flags);
```

**Impact on lobslaw:** When this lands (Go 1.27 or 1.28, probably), we can delete the landlock branch of the helper and the NoNewPrivs prctl call. The ruleset FD building via `landlock-lsm/go-landlock` stays. Our `sandbox.Policy` type stays the same; only `Apply` changes.

### [#3405](https://github.com/golang/go/issues/3405) — `syscall: support 'mode 2 seccomp' on Linux`

**State:** Open. Inactive. Last comment 2024, filed 2012.

**Impact on lobslaw:** Probably never lands. Seccomp install will remain the permanent reason for the helper binary even after #68595.

### No standalone NoNewPrivs proposal

NoNewPrivs is not getting its own `SysProcAttr.NoNewPrivs` field. It rides along with the Landlock proposal above because Landlock *requires* it. If #68595 lands, NoNewPrivs comes with it.

---

## What happens when stdlib lands

Migration path, when #68595 ships:

1. Upgrade `go.mod` to the minimum version containing the feature.
2. In `internal/sandbox/apply_linux.go`, **build the Landlock ruleset in the parent** (not the child) using the same go-landlock calls, but with an `AsRuleset()`-style modifier that returns the FD instead of calling `landlock_restrict_self`.
3. Pass the FD + flags via `cmd.SysProcAttr.LandlockRestrictSelfRulesetFD` and set `UseLandlockRestrictSelf = true`.
4. Drop the landlock + NoNewPrivs branches from the helper subcommand.
5. Keep the helper subcommand for seccomp only.

Caller-visible API of `sandbox.Policy` does not change.

---

## Policy attachment: where Policies live

A `sandbox.Policy` is a pure value type — no identity, no scope. Where it's attached is orthogonal:

| Attach point | Where | Purpose |
|---|---|---|
| `ExecutorConfig.Sandbox` | `internal/compute/executor.go` | Fleet-wide default — applied to any tool without a specific policy |
| `Registry.SetPolicy(name, *Policy)` | `internal/compute/registry.go` | Per-tool — the common case |
| `InvokeRequest.Sandbox` *(future)* | `internal/compute/executor.go` | Per-invocation override — deferred until a caller needs it |

`Executor.resolvePolicy` walks the chain: **tool-specific → fleet default → nil**. A nil Policy means "no sandbox" — `sandbox.Apply` is a no-op. A non-nil empty Policy (`&Policy{}`) short-circuits the chain, making it the way to say "this tool is explicitly unsandboxed even though the fleet sandboxes".

---

## Standard presets

Every preset is **read-only by default**. Operators compose explicit `path:rw` overrides for paths a tool needs to write to — least-privilege posture means the operator has to say "yes I really want this writable".

| Preset | Description | Paths |
|---|---|---|
| `system-libs` | OS executables + shared libraries (RO) | `/usr`, `/bin`, `/sbin`, `/lib`, `/lib64` |
| `system-certs` | TLS CA bundles for HTTPS (RO) | `/etc/ssl`, `/etc/ca-certificates`, `/etc/pki` |
| `dns` | DNS resolver + hosts file (RO) | `/etc/resolv.conf`, `/etc/nsswitch.conf`, `/etc/hosts` |
| `tmp` | `/tmp` scratch space (**RW**) | `/tmp` |
| `home-config` | User config dir (RO) | `~/.config` |
| `git-config` | Git config + global hooks (RO) | `~/.gitconfig`, `~/.config/git` |
| `ssh-keys` | SSH keys + known_hosts (RO) | `~/.ssh` |
| `gpg-keys` | GPG keyring (RO) | `~/.gnupg` |
| `aws-creds` | AWS credentials + config (RO) | `~/.aws` |

Defined in code (`internal/sandbox/preset.go` → `BuiltinPresets`). Operators extend/override via `.policy.toml` files in `policy.d/_presets/`.

### Path access notation: `path[:flags]`

| Suffix | Access | Landlock rule kind |
|---|---|---|
| *(none)* | read-only | `RODirs` / `ROFiles` |
| `:r` | read-only | `RODirs` / `ROFiles` |
| `:rw` | read + write | `RWDirs` / `RWFiles` |
| `:rx` | read + execute | `RODirs` (grants exec) |
| `:rwx` | full access | `RWDirs` (grants exec) |

Bare `w` or `x` without `r` is rejected — always a typo in practice.

### Composition semantics

Operating on the composed list of PathRules from all presets + inline paths:

1. **`~` expansion** happens at compose time via `os.UserHomeDir()` (the agent's UID's home — single-tenant model).
2. **Canonicalisation** via `filepath.EvalSymlinks`. Missing paths are silently dropped (matches landlock's `IgnoreIfMissing` posture).
3. **Longest-realpath wins** for nested paths — handled by landlock itself at kernel-level per-path rule evaluation. `/usr/local/app` overrides `/usr` for files inside it.
4. **Exact realpath duplicates** merge to the **most-permissive** access (RW beats R, RWX beats RW).

Resolved rules are returned sorted longest-path-first so debug logs and iteration read "most-specific first".

---

## The `.policy.toml` file format

Ship alongside skills or drop into a `policy.d/` directory. Filename convention: stem = tool name.

### Discovery model — multi-directory with layered precedence

**Policy directories are a list, not a single path.** The loader merges all of them with "later wins" semantics so operators can layer sources the way `git config` layers system/global/local.

**Default discovery** (when nothing is specified, in load order — earliest is lowest priority):

1. `$XDG_CONFIG_HOME/lobslaw/policy.d/` (or `~/.config/lobslaw/policy.d/`) — user-global
2. `<config-dir>/policy.d/` — derived from resolved `config.toml`
3. `<cwd>/policy.d/` — workspace-local

Duplicates are collapsed via `EvalSymlinks` (common in dev where `configDir == cwd`). Missing directories are skipped silently.

**Explicit lists skip the defaults entirely** — standard CLI ergonomics (*"if I set `--policy-dir`, don't sneak in extras"*). Three ways to set an explicit list, in precedence order:

| Source | How |
|---|---|
| **CLI (highest)** | `--policy-dir <path>` repeatable: `lobslaw --policy-dir /corp/defaults --policy-dir /srv/project/policy.d` |
| **Config** | `[sandbox] policy_dirs = ["/corp/defaults", "/srv/project/policy.d"]` — array of strings |
| **Env** | `LOBSLAW__SANDBOX__POLICY_DIRS=/corp/defaults,/srv/project/policy.d` |

Whichever is set at the highest-precedence layer wins and replaces the lower layers entirely. Within a single source, list order is load order; later dirs override earlier on same-tool conflicts.

```
<policy-dir>/
├── git.toml                 # applies to the "git" tool
├── rsync.toml
├── curl.toml
└── _presets/                # optional operator preset overrides
    ├── corp-certs.toml
    └── my-code.toml
```

Example: in a container at `/app/data` with user-config at `~/.config/lobslaw/policy.d/`, the default merge loads both; a site-wide `git.toml` in user-config can define baseline permissions, and a per-workspace `git.toml` in `/app/data/policy.d/` overrides specific entries.

### Tool policy schema

```toml
# <config-dir>/policy.d/git.toml
name = "git"                       # must match filename stem (if set)
description = "git operations with SSH/GPG/config access"

# Compose from presets (built-in or operator-defined)
presets = ["system-libs", "system-certs", "git-config", "ssh-keys", "dns"]

# Inline path entries; path[:flags] format. Flags default to "r".
paths = [
    "~/code:rw",
    "/tmp:rw",
]

no_new_privs = true                # strongly recommended
network_allow_cidr = ["0.0.0.0/0"] # enforcement deferred (nftables)

# Seccomp: either apply DefaultSeccompPolicy...
seccomp_default = true
# ...OR specify a custom deny list (mutually exclusive with above)
# seccomp_deny = ["ptrace", "mount", "init_module"]

[namespaces]
user = true
mount = true
pid = true
```

### Preset override / extension schema

```toml
# <config-dir>/policy.d/_presets/my-code.toml
name = "my-code"                   # must match filename stem
description = "My own source tree (RW)"
paths = [
    "/srv/code:rw",
    "~/personal-projects:rw",
]
```

Same-name override of a built-in is allowed; the loader logs it via `LoadResult.OverriddenBuiltins`.

### Load order + discovery

`sandbox.LoadPolicyDir(dir, opts)` (called at startup):

1. Load `_presets/*.toml` first → `RegisterPreset` each.
2. Load top-level `*.toml` → `PolicySpec.ToPolicy()` for each.

Ordering matters: tool policies in the same dir can reference presets defined in `_presets/`.

Missing `dir` is a no-op — callers can unconditionally point the loader at a path without feature-detecting whether it exists.

### Integrity check

Every `.policy.toml` file is verified before parsing. Failed files are logged at `WARN` and land in `LoadResult.Rejected`; siblings that pass continue to load (one bad file never poisons the whole directory).

| Check | Unix | Windows |
|---|---|---|
| **Owner UID** | Must match `LoadOptions.TrustedUID`. Default: `os.Geteuid()` — i.e., the agent's own UID. Set to `-1` to skip. | No-op (NTFS ACLs aren't inspected; protect the dir via filesystem permissions instead). |
| **Mode mask** | File must NOT have any bit in `LoadOptions.RejectWritableMask`. Default: `0o022` — rejects group-writable and world-writable. | No-op. |
| **Escape hatch** | `LoadOptions.SkipPermChecks = true` disables both checks (e.g. for k8s volume drivers that can't preserve Unix mode). | (Always-on — the check is already no-op on Windows.) |

Threat model: an attacker with write access to the policy directory at its own UID could otherwise drop a permissive policy on next hot-reload. The UID check defeats that ("only files I or someone with my privileges authored are trusted"). Same well-trodden pattern as OpenSSH's `~/.ssh/config` check and sudo's `/etc/sudoers` check.

### Hot-reload

The `sandbox.Watcher` wraps `fsnotify` with a 250ms debounce. Start with a `*PolicySink` (satisfied by `compute.Registry`) and a `context.Context`:

```go
w := sandbox.NewWatcher(dir, registry, opts, 0) // 0 = default 250ms debounce
w.Start(ctx)  // initial load + fsnotify subscribe
// ... ctx cancellation stops the watcher
```

- Editor write-bursts (vim's write-swap-rename) coalesce into one reload.
- Deleted files clear the per-tool policy via `SetPolicy(name, nil)` — the fleet default takes over again.
- Rejected files (perm check fail) log at WARN but don't stop siblings.
- Only tools the watcher previously loaded are mutated; skill-set policies remain untouched.

Disable via `[sandbox] hot_reload_opt_out = true` when you want load-once-at-boot behaviour (air-gapped deployments, `--read-only` containers where the policy dir can't change).

### Skill-bundled policies

Skills ship a `policy.d/` subtree identical in shape to the operator's top-level one. The loader exposes `sandbox.LoadSkillPolicies(skillPolicyDir, ownedTools, sink, opts)` which wraps `LoadPolicyDir` with an **ownership guard**: a skill may only ship policies for tools it actually registers. Policies for tool names outside `ownedTools` are logged and rejected.

Boot-time ordering ensures precedence — operator's authoritative policies always win:

1. Skills install → `LoadSkillPolicies` per skill.
2. Operator policy.d/ loads → `LoadPolicyDir` + apply to Registry. Overwrites any same-name entries.
3. Hot-reload watcher subscribes to operator policy.d/ only (skills register their policies once at install, not continuously).

---

## Recipes

Minimal examples for common tools. All assume `no_new_privs = true` and `seccomp_default = true` are set.

### `git` (read/write repo, SSH/GPG auth, HTTPS push)

```toml
name = "git"
presets = ["system-libs", "system-certs", "dns", "git-config", "ssh-keys", "gpg-keys"]
paths = ["~/code:rw", "/tmp:rw"]
network_allow_cidr = ["0.0.0.0/0"]

[namespaces]
user = true
mount = true
```

### `rsync` (backup to a mount)

```toml
name = "rsync"
presets = ["system-libs", "system-certs", "dns"]
paths = ["~/code:r", "/mnt/backup:rw", "/tmp:rw"]
network_allow_cidr = ["10.0.0.0/8"]
```

### `curl` (fetch public URLs, no file writes)

```toml
name = "curl"
presets = ["system-libs", "system-certs", "dns"]
paths = ["/tmp:rw"]
network_allow_cidr = ["0.0.0.0/0"]
```

### Owner-authored `bash` (unsandboxed — "I trust this")

```toml
name = "bash"
# No presets, no paths, no enforcement fields → short-circuits the
# fleet default and runs fully unsandboxed. Explicit override means
# the operator owns the decision.
```

### Untrusted `run_python` skill (tight bounds)

```toml
name = "run_python"
presets = ["system-libs"]
paths = ["/var/lobslaw/workspace:rw", "/tmp:rw"]
no_new_privs = true
seccomp_default = true

[namespaces]
user = true
mount = true
pid = true
network = true   # isolate — run_python gets no egress
```

---

## Why this deserves its own document

It isn't obvious from reading `internal/sandbox/` alone why we:

- Have a `sandbox-exec` subcommand wiring helper code into `cmd/lobslaw/`.
- Serialise `Policy` to JSON across the exec boundary when we already have the struct in-process.
- Write a helper at all when stdlib has `SysProcAttr`.

This file is the why. If #68595 or #3405 moves, update the Upstream Tracking section. If the helper pattern ever feels vestigial, check whether the proposals landed before deleting it.
