---
topic: lobslaw-sandbox-introspection
decision: "debug_sandbox builtin reports kernel sandbox capabilities: landlock_supported (LSM), landlock_abi_version, seccomp_supported, no_new_privs_supported, cgroup_v2_mounted, daemon_under_sandbox (whether the daemon itself runs under seccomp filter), and sandbox_mode ('enforces-tools' | 'none'). Probes are no-side-effect filesystem reads of /proc/self/status, /sys/kernel/security/landlock, /sys/fs/cgroup. Linux-only enforcement; non-Linux returns sandbox_mode='none' so operators can't be confused. Tool isolation applies to subprocess execution (shell_command, MCP servers, exec tools), not the daemon process itself."
date: 2026-04-24
---

# lobslaw-sandbox-introspection

**Decision:** debug_sandbox builtin reports kernel sandbox capabilities: landlock_supported (LSM), landlock_abi_version, seccomp_supported, no_new_privs_supported, cgroup_v2_mounted, daemon_under_sandbox (whether the daemon itself runs under seccomp filter), and sandbox_mode ('enforces-tools' | 'none'). Probes are no-side-effect filesystem reads of /proc/self/status, /sys/kernel/security/landlock, /sys/fs/cgroup. Linux-only enforcement; non-Linux returns sandbox_mode='none' so operators can't be confused. Tool isolation applies to subprocess execution (shell_command, MCP servers, exec tools), not the daemon process itself.

## Rationale

Operators couldn't verify sandbox enforcement was actually working — 'is landlock active?' had no answer. The debug_* family is the right home: read-only, scope-gateable via policy, plain markdown table renders cleanly. Probe approach over runtime tests because spawning a verifier subprocess to confirm syscall denial would itself be brittle and platform-dependent.

