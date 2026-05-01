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

.PHONY: update-dependencies
update-dependencies: ## Update Go module dependencies and refresh vendor hashes in flake.nix.
	go get -u ./...
	go mod tidy
	@update_vendor_hash() { \
	    var=$${1}VendorHash fake="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="; \
	    sed -i "s|$$var[[:space:]]*=[[:space:]]*\"sha256-[^\"]*\"|$$var = \"$$fake\"|" flake.nix; \
	    hash=$$(nix build .#$${1}-bin 2>&1 | awk '/got:/{print $$NF}' | head -1) || true; \
	    [ -n "$$hash" ] || { echo "error: could not determine $$var; restore flake.nix from git" >&2; return 1; }; \
	    sed -i "s|$$var = \"$$fake\"|$$var = \"$$hash\"|" flake.nix; \
	    echo "Updated $$var to $$hash"; \
	}; \
	update_vendor_hash watcher; \
	update_vendor_hash worker

# Detect hardware encoders via ffmpeg CLI — independent of our DetectHardwareEncoders() logic.
# This ensures hardware-specific build tags are set even if our detection has bugs, allowing
# tests to catch those bugs.
#
# detect_encoder checks whether ffmpeg exposes an h264 or hevc encoder for the
# given HW family (e.g. qsv, vaapi, nvenc). Outputs "1" if found, "0" otherwise.
detect_encoder = $(shell ffmpeg -hide_banner -encoders 2>/dev/null | \
	grep -qE '^\s+V..... (hevc|h264)_$(1)' && echo "1" || echo "0")

HAS_QSV_ENCODER := $(call detect_encoder,qsv)
HAS_VAAPI_ENCODER := $(call detect_encoder,vaapi)
HAS_NVENC_ENCODER := $(call detect_encoder,nvenc)
# HAS_HW_ENCODER is true when at least one of the per-family flags is set.
HAS_HW_ENCODER := $(if $(filter 1,$(HAS_QSV_ENCODER) $(HAS_VAAPI_ENCODER) $(HAS_NVENC_ENCODER)),1,0)

# Build tags: hwtest when any HW encoder is present; dedicated tags (qsvtest, vaapitest,
# nvenctest) when the corresponding hardware encoder is specifically present.
comma := ,
empty :=
space := $(empty) $(empty)
_test_tags := $(strip $(if $(filter 1,$(HAS_HW_ENCODER)),hwtest )$(if $(filter 1,$(HAS_QSV_ENCODER)),qsvtest )$(if $(filter 1,$(HAS_VAAPI_ENCODER)),vaapitest )$(if $(filter 1,$(HAS_NVENC_ENCODER)),nvenctest))
TEST_TAG_FLAGS := $(if $(_test_tags),-tags $(subst $(space),$(comma),$(_test_tags)))

.PHONY: test
test: fmt vet ## Run tests.
	go test -race -count=1 $(TEST_TAG_FLAGS) ./...

.PHONY: test-integration
test-integration: temporal-up ## Run integration tests against the local Temporal server (starts the server first).
	TEMPORAL_ADDRESS=localhost:7233 \
	TEMPORAL_NAMESPACE=default \
	TEMPORAL_TASK_QUEUE=media-processor-test \
	go test -v -race -count=1 -tags=integration ./...

.PHONY: test-e2e
test-e2e: build-images ## Run end-to-end tests (requires Docker; downloads ~700 MB BBB fixture on first run).
	go test -v -timeout 2h -tags=e2e -count=1 ./e2e/

.PHONY: benchmark
benchmark: ## Run benchmarks.
	go test -run='^$' -bench=. -benchtime=3x ./... | grep -i grep -i -e bench -e ns/op

.PHONY: lint
lint: ## Run golangci-lint.
	golangci-lint run ./...

.PHONY: lint-fix
lint-fix: ## Run golangci-lint and perform fixes.
	golangci-lint run --fix ./...

##@ Code Generation

.PHONY: generate-schema
generate-schema: ## Generate JSON schema for the watcher config file.
	@mkdir -p schemas
	go run ./cmd/gen-config-schema | jq > schemas/watcher.schema.json

##@ Container Images

VERSION = 0.0.1-dev
CONTAINER_REGISTRY = ghcr.io/soliddowant
PUSH_ALL ?= false

INCLUDE_LATEST = $(PUSH_ALL)

WATCHER_IMAGE_TAGS = $(CONTAINER_REGISTRY)/media-processor/watcher:$(VERSION) $(if $(filter true,$(INCLUDE_LATEST)),$(CONTAINER_REGISTRY)/media-processor/watcher:latest)
WORKER_IMAGE_TAGS = $(CONTAINER_REGISTRY)/media-processor/worker:$(VERSION) $(if $(filter true,$(INCLUDE_LATEST)),$(CONTAINER_REGISTRY)/media-processor/worker:latest)

.PHONY: build-images
build-images: ## Build watcher and worker OCI images and load them into the local Docker daemon.
	$$(nix build --print-out-paths --no-link .#watcher-image) | docker load
	$(foreach tag,$(WATCHER_IMAGE_TAGS),docker tag watcher:latest $(tag);)
	$$(nix build --print-out-paths --no-link .#worker-image) | docker load
	$(foreach tag,$(WORKER_IMAGE_TAGS),docker tag worker:latest $(tag);)
	$(if $(findstring t,$(PUSH_ALL)),$(foreach tag,$(WATCHER_IMAGE_TAGS) $(WORKER_IMAGE_TAGS),docker push $(tag);))

##@ Helm

HELM_CHART_DIR := $(PROJECT_DIR)/deploy/charts/media-processor
HELM_CHART_FILES := $(shell find $(HELM_CHART_DIR) -type f ! -path "*/charts/*")
HELM_REGISTRY := $(CONTAINER_REGISTRY)/charts
HELM_PACKAGE := $(BIN_DIR)/helm/media-processor-$(VERSION).tgz
HELM_PUSH ?= $(PUSH_ALL)

$(HELM_PACKAGE): $(HELM_CHART_FILES)
	@mkdir -p "$(@D)"
	helm package "$(HELM_CHART_DIR)" --dependency-update --version "$(VERSION)" --app-version "$(VERSION)" --destination "$(@D)"
	$(if $(findstring t,$(HELM_PUSH)),helm push "$(HELM_PACKAGE)" oci://$(HELM_REGISTRY))

.PHONY: helm
helm: $(HELM_PACKAGE) ## Package (and optionally push) the Helm chart.

##@ Build

# Note: CGO is required. FFmpeg 8 development headers must be available via pkg-config.
# Run 'nix develop' to enter the dev shell with all required dependencies.

$(BIN_DIR)/watcher: $(GO_SOURCE_FILES)
	@mkdir -p "$(BIN_DIR)"
	go build -ldflags="-s -w" -o "$@" ./cmd/watcher

$(BIN_DIR)/worker: $(GO_SOURCE_FILES)
	@mkdir -p "$(BIN_DIR)"
	go build -ldflags="-s -w" -o "$@" ./cmd/worker

.PHONY: build
build: $(BIN_DIR)/watcher $(BIN_DIR)/worker ## Build all binaries.

##@ Release

.PHONY: build-all
build-all: build-images helm ## Build all release artifacts (and push images/chart when PUSH_ALL=true).

.PHONY: release
release: TAG = v$(VERSION)
release: SAFETY_PREFIX = $(if $(findstring t,$(PUSH_ALL)),,echo)
release: build-all ## Create a GitHub release. Set PUSH_ALL=true to tag, push, and publish. Requires the GitHub CLI (gh).
	@gh auth status
	@$(SAFETY_PREFIX) git tag -a $(TAG) -m "Release $(TAG)"
	@$(SAFETY_PREFIX) git push origin
	@$(SAFETY_PREFIX) git push origin --tags
	@$(SAFETY_PREFIX) gh release create $(TAG) --generate-notes --latest --verify-tag

.PHONY: clean
clean: INCLUDE_LATEST = true
clean: ## Clean up all build artifacts and loaded container images.
	@rm -rf $(BIN_DIR) $(HELM_CHART_DIR)/charts
	@docker image rm -f $(WATCHER_IMAGE_TAGS) $(WORKER_IMAGE_TAGS) 2>/dev/null >/dev/null || true

##@ Local Dev

.PHONY: temporal-up
temporal-up: ## Start the local Temporal dev stack (server + Postgres + Web UI).
	docker compose up -d --wait
	@echo "Temporal is ready. gRPC: localhost:7233  Web UI: http://localhost:8080  Namespace: default"

.PHONY: temporal-down
temporal-down: ## Stop the local Temporal dev stack and remove volumes/networks.
	docker compose down -v --remove-orphans
