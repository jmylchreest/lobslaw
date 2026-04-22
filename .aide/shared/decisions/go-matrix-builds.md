---
topic: go-matrix-builds
decision: "Use explicit matrix includes for cross-platform builds; zig for Linux/Windows cross-compilation, native runners for macOS"
decided_by: blueprint:go-github-actions@0.0.59
date: 2026-04-21
---

# go-matrix-builds

**Decision:** Use explicit matrix includes for cross-platform builds; zig for Linux/Windows cross-compilation, native runners for macOS

## Rationale

Explicit include entries per target give precise control over runner, compiler, and flags for each platform/arch combination. Zig provides a single-binary C cross-compiler that targets all platforms from a Linux runner. macOS builds use native clang because macOS SDK headers are required.

## Details

6-target matrix (fail-fast: false): linux/{amd64,arm64} ubuntu+zig, darwin/{amd64,arm64} macos+clang, windows/{amd64,arm64} ubuntu+zig. CGO_ENABLED=1, CC='zig cc -target ${{ matrix.zig_target }}'. .exe for Windows. Naming: {binary}-{os}-{arch}[.exe]. UPX optional (skip darwin, windows/arm64). Use over GoReleaser when CGO needed.

