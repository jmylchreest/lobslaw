.PHONY: proto proto-lint proto-breaking proto-tools build test smoke lint lint-tools tidy hooks hook-tools

# Go-tool-installed binaries live under $(go env GOPATH)/bin. Prepend to PATH
# for targets that shell out to them (buf invokes protoc-gen-go via PATH).
GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

# Pin protoc-gen-* tool versions here; update alongside google.golang.org/protobuf
# and google.golang.org/grpc in go.mod.
# Keep in step with the version pinned in .github/workflows/lint.yml,
# so a local `make lint` reports what CI will.
GOLANGCI_LINT_VERSION      := v2.13.2

# The Go toolchain the linter is built with AND run under, taken from
# go.mod so it cannot drift from CI, which selects the same one via
# `go-version-file: go.mod`.
#
# Pinning the linter version alone was not enough. golangci-lint bundles
# staticcheck, whose IR builder has to understand the stdlib SOURCE it
# parses, so a distro Go newer than the bundled staticcheck supports
# makes it panic ("unexpected expr: *ast.KeyValueExpr" on internal/poll)
# rather than report findings. The failure is quiet in the worst way:
# `make lint` exits non-zero having run none of the staticcheck or revive
# checks, so a contributor on a newer Go sees a crash they assume is
# environmental, skips it, and CI finds the real issues instead.
LINT_TOOLCHAIN             := go$(shell awk '/^go /{print $$2; exit}' go.mod)
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
	@GOTOOLCHAIN=$(LINT_TOOLCHAIN) go vet ./...
	@gofmt -l . | (! grep .) || (echo "gofmt needed on files above" && exit 1)
	@GOTOOLCHAIN=$(LINT_TOOLCHAIN) golangci-lint config verify
	@GOTOOLCHAIN=$(LINT_TOOLCHAIN) golangci-lint run ./...

# Pinned so `make lint` reports exactly what CI reports. Unlike the
# secret scanner, which floats at @latest: a stale style linter is
# harmless, a stale secret scanner is a missed credential.
lint-tools:
	@GOTOOLCHAIN=$(LINT_TOOLCHAIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

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
# Built with HOOK_TOOLCHAIN rather than the repo's Go, because these are
# tools and not the product: what compiles them is incidental, and one of
# them transitively depends on go-json-experiment/json, whose older
# versions alias encoding/json/v2 symbols (SkipFunc,
# DiscardUnknownMembers) that were removed before Go 1.27 shipped. That
# combination does not compile, and it took out the secret scan — the one
# check whose job is to keep credentials out of the repository.
#
# Pinned rather than diagnosed further because the scanner floats at
# @latest deliberately (see above) and its dependency graph is not ours
# to fix. Drop this and let it follow LINT_TOOLCHAIN once betterleaks
# builds clean on the repo's Go.
HOOK_TOOLCHAIN := go1.26.2

hook-tools:
	@GOTOOLCHAIN=$(HOOK_TOOLCHAIN) go install github.com/evilmartians/lefthook@latest
	@GOTOOLCHAIN=$(HOOK_TOOLCHAIN) go install github.com/betterleaks/betterleaks@latest
