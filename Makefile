.PHONY: proto proto-lint proto-breaking proto-tools build test lint tidy

# Go-tool-installed binaries live under $(go env GOPATH)/bin. Prepend to PATH
# for targets that shell out to them (buf invokes protoc-gen-go via PATH).
GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

# Pin protoc-gen-* tool versions here; update alongside google.golang.org/protobuf
# and google.golang.org/grpc in go.mod.
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

lint:
	@go vet ./...
	@gofmt -l . | (! grep .) || (echo "gofmt needed on files above" && exit 1)

tidy:
	@go mod tidy
