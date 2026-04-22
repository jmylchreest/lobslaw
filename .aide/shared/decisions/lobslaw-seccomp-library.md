---
topic: lobslaw-seccomp-library
decision: "elastic/go-seccomp-bpf (pure Go, no CGO). Emits BPF filters directly from allow/deny policies. Used in production by Elastic Beats. Rejected: seccomp/libseccomp-golang (CGO bindings to the C libseccomp library - conflicts with go-cgo decision that prefers CGO_ENABLED=0 for static binaries and simpler cross-compilation)"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-seccomp-library

**Decision:** elastic/go-seccomp-bpf (pure Go, no CGO). Emits BPF filters directly from allow/deny policies. Used in production by Elastic Beats. Rejected: seccomp/libseccomp-golang (CGO bindings to the C libseccomp library - conflicts with go-cgo decision that prefers CGO_ENABLED=0 for static binaries and simpler cross-compilation)

## Rationale

go-cgo decision is explicit: CGO_ENABLED=0 unless there's no alternative. There IS an alternative for seccomp - elastic/go-seccomp-bpf is pure Go and battle-tested by Elastic. Picking it keeps the static-binary, simple-cross-compile property across all platforms

