# syntax=docker/dockerfile:1.7
#
# Core lobslaw node image — distroless, ~10MB, no shell.
#
# For nodes that do NOT execute local skill/tool subprocesses
# (memory, policy, storage, or gateway without local skill runtimes),
# this image is the right default: minimal attack surface, nonroot,
# no package manager, no shell.
#
# Compute nodes that run Phase 8 python/bash skill runtimes should
# use Dockerfile.tools instead — distroless has no interpreters.

ARG GO_VERSION=1.26

# ---- Tiny shell stage ----------------------------------------------
# uv's libc-detection probe reads PT_INTERP from /bin/sh. The
# distroless static base has no /bin contents at all, and busybox-
# static (in the :debug variants) is statically linked so PT_INTERP
# is empty. Smallest fix: extract `dash` (~140KB dynamic binary)
# from debian-slim and copy it into the runtime image. dash links
# against glibc which we get from distroless/base.
FROM debian:12-slim AS shell
RUN apt-get update && apt-get install -y --no-install-recommends libgcc-s1 libstdc++6 && \
    mkdir -p /sysroot/bin /sysroot/lib/x86_64-linux-gnu && \
    cp /bin/dash /sysroot/bin/sh && \
    # Python C extensions (pydantic_core, etc.) link against libgcc_s
    # and libstdc++. distroless/base supplies libc but not these.
    cp -L /lib/x86_64-linux-gnu/libgcc_s.so.1 \
          /lib/x86_64-linux-gnu/libstdc++.so.6 \
          /sysroot/lib/x86_64-linux-gnu/

# ---- Build stage ---------------------------------------------------
FROM golang:${GO_VERSION} AS build

WORKDIR /src

# Module cache layer: only invalidates when go.mod / go.sum change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown

# Pure-Go build: CGO_ENABLED=0 so the binary is statically linked
# and runs on distroless/static. -trimpath strips local paths for
# reproducibility; -s -w drops debug info (~30% smaller binary).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT}" \
        -o /out/lobslaw \
        ./cmd/lobslaw

# ---- Runtime stage -------------------------------------------------
# gcr.io/distroless/base-debian12:debug-nonroot includes:
#   - glibc + dynamic linker (/lib64/ld-linux-x86-64.so.2)
#   - busybox at /busybox/ for shell + utilities
#   - dynamically-linked /bin/sh (CRITICAL: uv reads its ELF
#     PT_INTERP segment to detect the system's libc variant before
#     downloading a Python interpreter; static busybox has no
#     PT_INTERP, so the static-debian12 variant fails uv's probe)
#   - CA certs at /etc/ssl/certs (outbound HTTPS to LLM providers)
#   - tzdata, /tmp, /etc/passwd entries for nonroot
# Trade: ~25MB total (vs 10MB for static-only). We accept this so
# lobslaw can drive `uv tool install` / `uvx` directly at runtime,
# which is what the [[mcp.servers]] install field needs.
FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=build /out/lobslaw /usr/local/bin/lobslaw

# Tiny dynamic /bin/sh + libgcc_s + libstdc++ extracted from
# debian:slim. `/bin/sh` is needed so uv's libc-detection probe (used
# by `uv tool install` to pick the right python-build-standalone
# variant) can read PT_INTERP. The libs are needed by Python C
# extensions like pydantic_core (Rust). distroless/base already
# supplies the matching glibc + ld-linux.
COPY --from=shell /sysroot/bin/sh /bin/sh
COPY --from=shell /sysroot/lib/x86_64-linux-gnu/libgcc_s.so.1 /lib/x86_64-linux-gnu/
COPY --from=shell /sysroot/lib/x86_64-linux-gnu/libstdc++.so.6 /lib/x86_64-linux-gnu/

USER nonroot:nonroot
WORKDIR /lobslaw

# gRPC cluster control plane (mTLS)
EXPOSE 7443
# Gateway HTTP (REST + Telegram webhook) — TLS terminated in-process
EXPOSE 8443
# UDP auto-discovery (opt-in via [discovery].broadcast = true)
EXPOSE 7445/udp

ENTRYPOINT ["/usr/local/bin/lobslaw"]
