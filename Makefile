.PHONY: proto proto-lint proto-breaking proto-tools build test lint lint-tools tidy

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
