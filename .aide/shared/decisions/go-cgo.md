---
topic: go-cgo
decision: "Prefer CGO_ENABLED=0 for pure Go; when CGO is required, document why and use zig for cross-compilation"
decided_by: blueprint:go@0.0.59
date: 2026-04-21
references:
  - https://go.dev/doc/install/source#environment
  - https://ziglang.org/
---

# go-cgo

**Decision:** Prefer CGO_ENABLED=0 for pure Go; when CGO is required, document why and use zig for cross-compilation

## Rationale

Pure Go (CGO_ENABLED=0) produces static binaries with no libc dependency, trivial cross-compilation, and simpler CI. But some dependencies require CGO (tree-sitter, SQLite, etc.). When CGO is necessary, zig provides a drop-in C/C++ cross-compiler that targets all major platforms from a single CI runner.

## Details

- Default CGO_ENABLED=0 for static binaries.
- CGO required: document why. CC='zig cc -target x86_64-linux-gnu' for Linux/Windows. Native clang on macOS.
- Test CGO on all target platforms in CI. Strip with -s -w ldflags.
- Naming: {binary}-{os}-{arch}[.exe]. UPX optional (skip darwin).

