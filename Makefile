SHELL := /bin/bash

MODULE   := github.com/paingha/tugcli
BIN      := bin/tug
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse HEAD 2>/dev/null)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w \
	-X $(MODULE)/internal/version.version=$(VERSION) \
	-X $(MODULE)/internal/version.commit=$(COMMIT) \
	-X $(MODULE)/internal/version.date=$(DATE)

GOLANGCI_LINT ?= golangci-lint
KIND_CLUSTER  ?= tug-e2e

.PHONY: all
all: lint test build

.PHONY: build
build: ## Build bin/tug
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/tug

.PHONY: example-plugin
example-plugin: ## Build the example provider plugin
	go build -o examples/tug-provider-example/bin/tug-provider-example ./examples/tug-provider-example

.PHONY: example-plugin-wasm
example-plugin-wasm: ## Build the example provider plugin as WebAssembly, for every platform
	GOOS=wasip1 GOARCH=wasm go build -trimpath -o examples/tug-provider-example/bin/tug-provider-example.wasm ./examples/tug-provider-example

PLUGINS        := akamai coreweave crusoe digitalocean nebius rancher tailscale teleport vultr
PLUGIN_HOST    := $(shell go env GOOS)_$(shell go env GOARCH)
PLUGIN_EXT     := $(if $(filter windows,$(shell go env GOOS)),.exe,)
PLUGIN_TARGETS := darwin_arm64 darwin_amd64 linux_amd64 linux_arm64 windows_amd64

.PHONY: plugins
plugins: ## Build the first-party plugins for this machine
	@for p in $(PLUGINS); do \
		CGO_ENABLED=0 go build -trimpath -o plugins/$$p/bin/$(PLUGIN_HOST)/tug-provider-$$p$(PLUGIN_EXT) ./plugins/$$p/cmd/tug-provider-$$p || exit 1; \
	done

.PHONY: plugins-dist
plugins-dist: build ## Build the plugins for every platform and archive them in dist/plugins
	@for p in $(PLUGINS); do \
		for t in $(PLUGIN_TARGETS); do \
			os=$${t%_*}; arch=$${t#*_}; ext=; [ $$os = windows ] && ext=.exe; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -o plugins/$$p/bin/$$t/tug-provider-$$p$$ext ./plugins/$$p/cmd/tug-provider-$$p || exit 1; \
		done; \
		$(BIN) plugin package plugins/$$p -o dist/plugins || exit 1; \
	done

.PHONY: test
test: ## Run unit and in-process end-to-end tests
	go test -race ./...

.PHONY: test-short
test-short: ## Skip tests that build binaries
	go test -short ./...

PYTHON ?= python3

.PHONY: python-sdk-test
python-sdk-test: ## Run the Python SDK's unit tests
	cd sdk/python && PYTHONDONTWRITEBYTECODE=1 $(PYTHON) -m unittest discover -v

.PHONY: cover
cover: ## Write coverage.html
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

.PHONY: lint
lint: ## Run golangci-lint
	$(GOLANGCI_LINT) run ./...

.PHONY: fmt
fmt: ## Format code
	gofmt -w .
	$(GOLANGCI_LINT) fmt ./...

.PHONY: tidy
tidy: ## Tidy go.mod and fail if it changed
	go mod tidy
	git diff --exit-code go.mod go.sum

.PHONY: e2e-kind
e2e-kind: build ## Create a kind cluster and run the end-to-end script against it
	kind get clusters | grep -qx $(KIND_CLUSTER) || kind create cluster --name $(KIND_CLUSTER) --wait 60s
	TUG_BIN=$(abspath $(BIN)) KIND_CLUSTER=$(KIND_CLUSTER) ./scripts/e2e-kind.sh

.PHONY: snapshot
snapshot: ## Build release artifacts locally without publishing
	goreleaser release --snapshot --clean

.PHONY: clean
clean:
	rm -rf bin dist coverage.out coverage.html examples/tug-provider-example/bin

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-16s %s\n", $$1, $$2}'
