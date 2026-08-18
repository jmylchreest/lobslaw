.PHONY: proto proto-lint proto-breaking proto-tools build test smoke lint lint-tools tidy hooks hook-tools

# Go-tool-installed binaries live under $(go env GOPATH)/bin. Prepend to PATH
# for targets that shell out to them (buf invokes protoc-gen-go via PATH).
GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

# Pin protoc-gen-* tool versions here; update alongside google.golang.org/protobuf
# and google.golang.org/grpc in go.mod.
# Keep in step with the version pinned in .github/workflows/lint.yml,
# so a local `make lint` reports what CI will.
GOLANGCI_LINT_VERSION      := v2.12.2
PROTOC_GEN_GO_VERSION      := v1.36.11
PROTOC_GEN_GO_GRPC_VERSION := v1.5.1

proto-tools:
	@go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	@go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

proto: proto-tools
	@buf generate

proto-lint:
	@buf lint

# Detect proto breaking changes against main. Only meaningful on PR branches.
proto-breaking:
	@buf breaking --against '.git#branch=main'

build:
	@go build ./...

test:
	@go test -race -cover ./...

# Boots a real node and enrols an operator end to end.
#
# Separate from `test` because it binds ports and takes ~30s, and
# because it answers a different question: `test` asks whether the
# pieces behave, this asks whether the path works. R28 and R29 shipped
# with a green `test` and a flow that failed before its first RPC.
#
# Hermetic — its own temp dir, HOME and XDG_CONFIG_HOME, loopback only.
# Override SMOKE_CLUSTER_PORT / SMOKE_ENROL_PORT / SMOKE_GATEWAY_PORT
# if those ports are taken.
smoke:
	@./scripts/smoke.sh

# `config verify` runs first because the GitHub action does the same, and it
# rejects config keys that `golangci-lint run` silently tolerates — without it
# an invalid key passes locally and fails CI.
lint: lint-tools
	@go vet ./...
	@gofmt -l . | (! grep .) || (echo "gofmt needed on files above" && exit 1)
	@golangci-lint config verify
	@golangci-lint run ./...

# Pinned so `make lint` reports exactly what CI reports. Unlike the
# secret scanner, which floats at @latest: a stale style linter is
# harmless, a stale secret scanner is a missed credential.
lint-tools:
	@go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

tidy:
	@go mod tidy

# Git never installs hooks from a clone (a repo that could run code on clone
# would be a supply-chain hole), so point git at the committed .githooks/ dir.
# Run once per checkout; re-running is harmless.
hooks: hook-tools
	@git config --unset-all core.hooksPath 2>/dev/null || true
	@lefthook install
	@echo "git hooks installed (lefthook)"

# lefthook dispatches the hooks; betterleaks is what the secret-scan job runs.
# The scan fails rather than skipping when its binary is missing, so installing
# it is part of installing the hooks, not a separate step people discover from
# an error message.
#
# core.hooksPath is unset first: it used to point at a committed .githooks/
# directory, and while it is set git ignores the .git/hooks/ scripts lefthook
# writes — so a checkout that ran the old `make hooks` would silently get no
# hooks at all.
#
# Deliberately @latest, unlike the protoc-gen-* pins above: a secret scanner is
# only as good as its newest rules, and pinning one freezes detection at the
# day someone chose the number. A surprise finding costs a minute; a missed
# credential in a public repo does not. (A minimum-version floor isn't an
# option either — `go install` builds report their version as "dev", since the
# real one is stamped by the upstream release build.)
hook-tools:
	@go install github.com/evilmartians/lefthook@latest
	@go install github.com/betterleaks/betterleaks@latest
