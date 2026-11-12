# Tool versions are pinned so generated code is the same on every machine.
BUF_VERSION           := v1.75.0
PROTOC_GEN_GO_VERSION := v1.36.12
CONNECT_VERSION       := v2.0.2
STATICCHECK_VERSION   := v0.8.2

GO      ?= go
BIN     := $(CURDIR)/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# The race detector needs cgo and a C compiler; run `make test TESTFLAGS=` without one.
TESTFLAGS ?= -race

export PATH := $(BIN):$(PATH)

.PHONY: tools generate lint test test-integration build run clean

## tools: install buf, the protobuf code generators and staticcheck into ./bin
tools:
	GOBIN=$(BIN) $(GO) install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	GOBIN=$(BIN) $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN=$(BIN) $(GO) install connectrpc.com/connect/v2/cmd/protoc-gen-connect-go@$(CONNECT_VERSION)
	GOBIN=$(BIN) $(GO) install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)

## generate: regenerate Go code from reporter-go/proto
generate:
	cd reporter-go && buf generate

## lint: lint the proto files and both Go modules
lint:
	cd reporter-go && buf lint
	$(GO) vet ./...
	cd reporter-go && $(GO) vet ./...
	staticcheck ./...
	cd reporter-go && staticcheck ./...

## test: run unit tests (MongoDB tests are skipped unless REPORTER_TEST_MONGO_URI is set)
test:
	$(GO) test $(TESTFLAGS) ./...
	cd reporter-go && $(GO) test $(TESTFLAGS) ./...

## test-integration: run every test, including the ones that need MongoDB
test-integration:
	@test -n "$(REPORTER_TEST_MONGO_URI)" || { echo "set REPORTER_TEST_MONGO_URI, for example mongodb://127.0.0.1:27017"; exit 1; }
	$(GO) test $(TESTFLAGS) -count=1 ./...
	cd reporter-go && $(GO) test $(TESTFLAGS) -count=1 ./...

## build: build the reporter service binary into ./bin/reporter
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN)/reporter ./cmd/reporter

## run: build and start the service with the development config
run: build
	$(BIN)/reporter serve -config deploy/config.dev.json

clean:
	rm -rf $(BIN)
