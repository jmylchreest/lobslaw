# syntax=docker/dockerfile:1.7
#
# Core lobslaw node image — alpine 3.20, ~15MB, full POSIX shell.
#
# Why alpine over distroless: alpine ships busybox + a real package
# manager (apk) baked in. That gives us /bin/sh + 300 standard tools,
# making curl-sh installer scripts (uv, bun, etc.) work directly
# inside the runtime container without a separate busybox sidecar
# volume. apk is also exposed as an install manager to the binary
# registry, so clawhub bundles declaring `kind: apk` install paths
# can satisfy bin requirements at runtime when lobslaw runs as root
# in the container.
#
# Compute nodes that run Phase 8 python/bash skill runtimes can use
# Dockerfile.tools for a heavier image with bash/git/python3/uv/bun/
# curl pre-installed. This file's runtime image is the lighter one
# for nodes whose skills only need POSIX baseline + apk-installable
# packages.

ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.20

# ---- Build stage ---------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

# Module cache layer: only invalidates when go.mod / go.sum change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown

# Pure-Go build: CGO_ENABLED=0 so the binary is statically linked
# and runs against musl on alpine without dynamic linking surprises.
# -trimpath strips local paths for reproducibility; -s -w drops debug
# info (~30% smaller binary).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT}" \
        -o /out/lobslaw \
        ./cmd/lobslaw

# ---- Runtime stage -------------------------------------------------
FROM alpine:${ALPINE_VERSION}

# Baseline runtime tools:
#   ca-certificates: outbound HTTPS to LLM providers, clawhub, OAuth.
#   tzdata: scheduler timezone parsing.
#   libgcc, libstdc++: Python C extensions uv may install (pydantic
#                      Rust core, etc.) link against them.
# busybox + apk are already in the alpine base — no extra install.
RUN apk add --no-cache \
        ca-certificates \
        tzdata \
        libgcc \
        libstdc++ && \
    addgroup -S -g 65532 nonroot && \
    adduser -S -u 65532 -G nonroot -h /lobslaw -s /sbin/nologin nonroot && \
    mkdir -p /lobslaw/usr/local/bin && \
    chown -R 65532:65532 /lobslaw

COPY --from=build /out/lobslaw /usr/local/bin/lobslaw

USER nonroot:nonroot
WORKDIR /lobslaw

# gRPC cluster control plane (mTLS)
EXPOSE 7443
# Gateway HTTP (REST + Telegram webhook) — TLS terminated in-process
EXPOSE 8443
# UDP auto-discovery (opt-in via [discovery].broadcast = true)
EXPOSE 7445/udp

ENTRYPOINT ["/usr/local/bin/lobslaw"]
