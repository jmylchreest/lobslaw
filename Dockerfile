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
# gcr.io/distroless/static-debian12:nonroot ships:
#   - CA certs at /etc/ssl/certs (outbound HTTPS to LLM providers)
#   - tzdata (scheduler cron parsing honours operator timezone)
#   - /tmp (lumberjack rotation staging)
#   - user "nonroot" UID 65532
# No shell, no package manager, no ldd — reduces the attack surface
# for a long-running service.
FROM gcr.io/distroless/static-debian12:nonroot

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
