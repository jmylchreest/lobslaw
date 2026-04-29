# syntax=docker/dockerfile:1.7
#
# Core lobslaw node image — debian:12-slim, ~80MB, glibc.
#
# We pivoted from alpine (~34MB) to debian-slim because the install
# manager ecosystem skill bundles target (brew, uv, bun, ruby gems)
# is glibc-built and doesn't run cleanly on musl. Trade ~46MB extra
# for compatibility with the install scripts the skills expect.
#
# Tools available natively in debian-slim:
#   /bin/sh (dash) + /bin/bash + standard GNU userland (grep, sed,
#   awk, tar, gzip — full GNU versions, not busybox).
# Things we add via apt:
#   ca-certificates: outbound HTTPS to LLM providers, clawhub, OAuth.
#   tzdata: scheduler timezone parsing.
#   curl: many install scripts shell out to curl explicitly.
#   procps + file: brew's install.sh sanity-checks need these.
#
# Compute nodes that run Phase 8 python/bash skill runtimes with
# heavy toolchains (git, python3, ruby) should still use
# Dockerfile.tools — that ships ~120MB with the broader runtime.

ARG GO_VERSION=1.26

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
# and runs against any libc without dynamic linking surprises.
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
FROM debian:12-slim

RUN apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates \
        tzdata \
        curl \
        procps \
        file \
        git && \
    rm -rf /var/lib/apt/lists/* && \
    groupadd --system --gid 65532 nonroot && \
    useradd --system --uid 65532 --gid 65532 \
        --home-dir /lobslaw --create-home --shell /usr/sbin/nologin \
        nonroot && \
    mkdir -p /lobslaw/usr/local/bin && \
    chown -R 65532:65532 /lobslaw
# git is in the apt-install above because brew's bootstrap clones
# the brew repo (and homebrew-core tap) via git rather than curl+tar.
# Brew installs at /lobslaw/usr/local (Satisfier's prefix); the manual
# git-clone bootstrap path skips install.sh's hardcoded /home/linuxbrew
# default. Trade-off: bottle-incompatibility for many formulae —
# they'll source-build. For Go-only formulae (gog) this is fine.

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
