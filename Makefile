SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -euc

PROJECT_DIR := $(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))
MODULE_NAME := $(shell go list -m)

BIN_DIR := $(PROJECT_DIR)/bin

GO_SOURCE_FILES := $(shell find cmd pkg \( -name '*.go' ! -name '*_test.go' \) 2>/dev/null)

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: fmt vet ## Run tests.
	go test -race -count=1 ./...

.PHONY: lint
lint: ## Run golangci-lint.
	golangci-lint run ./...

.PHONY: lint-fix
lint-fix: ## Run golangci-lint and perform fixes.
	golangci-lint run --fix ./...

##@ Build

$(BIN_DIR)/watcher: $(GO_SOURCE_FILES)
	@mkdir -p "$(BIN_DIR)"
	go build -o "$@" ./cmd/watcher

$(BIN_DIR)/worker: $(GO_SOURCE_FILES)
	@mkdir -p "$(BIN_DIR)"
	go build -o "$@" ./cmd/worker

.PHONY: build
build: $(BIN_DIR)/watcher $(BIN_DIR)/worker ## Build all binaries.
