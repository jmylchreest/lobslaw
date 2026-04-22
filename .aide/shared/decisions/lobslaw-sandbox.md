---
topic: lobslaw-sandbox
decision: "Native sandboxing enforced via: Linux mount+pid+net+user namespaces (unshare -m -p -n --user), seccomp filter with deny-by-default syscall policy, no_new_privs, cgroup v2 for cpu and memory quotas, nftables rules inside the netns for CIDR egress enforcement. Path allowlists enforced via realpath canonicalisation + prefix check, tool open uses O_NOFOLLOW. env_whitelist gates which env vars the tool sees"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-sandbox

**Decision:** Native sandboxing enforced via: Linux mount+pid+net+user namespaces (unshare -m -p -n --user), seccomp filter with deny-by-default syscall policy, no_new_privs, cgroup v2 for cpu and memory quotas, nftables rules inside the netns for CIDR egress enforcement. Path allowlists enforced via realpath canonicalisation + prefix check, tool open uses O_NOFOLLOW. env_whitelist gates which env vars the tool sees

## Rationale

Original decision named the tools (seccomp/bubblewrap) but did not specify enforcement mechanism. This makes it crisp: what namespaces, what syscall policy, what cgroup version, what firewall. 'Symlink traversal blocked' replaced with concrete realpath+O_NOFOLLOW pattern to avoid TOCTOU

