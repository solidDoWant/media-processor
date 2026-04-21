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
update-dependencies: ## Update Go module dependencies and sync Hatchet Docker image versions.
	go get -u ./...
	go mod tidy
	@HATCHET_VERSION=$$(go list -m github.com/hatchet-dev/hatchet | awk '{print $$2}'); \
	sed -i "s|ghcr\.io/hatchet-dev/hatchet/\([^:]*\):v[0-9][0-9.]*|ghcr.io/hatchet-dev/hatchet/\1:$${HATCHET_VERSION}|g" \
		docker-compose.yml \
		e2e/docker-compose.yml

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
test-integration: hatchet-up ## Run integration tests against a local Hatchet server (starts server, generates token).
	env $$(cat $(HATCHET_ENV_FILE)) go test -v -race -count=1 -tags=integration ./...

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

WATCHER_IMAGE_TAGS = $(CONTAINER_REGISTRY)/watcher:$(VERSION) $(if $(filter true,$(INCLUDE_LATEST)),$(CONTAINER_REGISTRY)/watcher:latest)
WORKER_IMAGE_TAGS = $(CONTAINER_REGISTRY)/worker:$(VERSION) $(if $(filter true,$(INCLUDE_LATEST)),$(CONTAINER_REGISTRY)/worker:latest)

.PHONY: build-images
build-images: ## Build watcher and worker OCI images and load them into the local Docker daemon.
	$$(nix build --print-out-paths --no-link .#watcher-image) | docker load
	$(foreach tag,$(WATCHER_IMAGE_TAGS),docker tag watcher:latest $(tag);)
	$$(nix build --print-out-paths --no-link .#worker-image) | docker load
	$(foreach tag,$(WORKER_IMAGE_TAGS),docker tag worker:latest $(tag);)
	$(if $(findstring t,$(PUSH_ALL)),$(foreach tag,$(WATCHER_IMAGE_TAGS) $(WORKER_IMAGE_TAGS),docker push $(tag);))

##@ Build

# Note: CGO is required. FFmpeg 8 development headers must be available via pkg-config.
# Run 'nix develop' to enter the dev shell with all required dependencies.

$(BIN_DIR)/watcher: $(GO_SOURCE_FILES)
	@mkdir -p "$(BIN_DIR)"
	go build -o "$@" ./cmd/watcher

$(BIN_DIR)/worker: $(GO_SOURCE_FILES)
	@mkdir -p "$(BIN_DIR)"
	go build -o "$@" ./cmd/worker

.PHONY: build
build: $(BIN_DIR)/watcher $(BIN_DIR)/worker ## Build all binaries.

##@ Local Dev

HATCHET_ENV_FILE := .env.hatchet

.PHONY: hatchet-up
hatchet-up: ## Start Hatchet local dev server and generate API token (written to .env.hatchet).
	docker compose up -d
	@echo "Waiting for Hatchet setup-config to complete..."
	@docker wait media-processor-setup-config-1
	@if [ ! -f $(HATCHET_ENV_FILE) ]; then $(MAKE) hatchet-token; fi
	@echo "Hatchet is ready. Dashboard: http://localhost:8080 (admin@example.com / Admin123!!)"
	@echo "Run 'source $(HATCHET_ENV_FILE)' to load HATCHET_CLIENT_TOKEN into your current shell."

.PHONY: hatchet-down
hatchet-down: ## Stop Hatchet local dev server.
	docker compose down

.PHONY: hatchet-token
hatchet-token: ## Generate a new Hatchet API token and write it to .env.hatchet.
	@echo "Generating Hatchet API token..."
	@TENANT_ID=$$(docker compose exec -T postgres \
		psql -U hatchet -d hatchet -t -c \
		"SELECT id FROM \"Tenant\" WHERE slug = 'default' LIMIT 1" \
		2>/dev/null | tr -d ' \n'); \
	if [ -z "$$TENANT_ID" ]; then \
		echo "Error: could not query tenant ID — is Hatchet running? Try: make hatchet-up" >&2; \
		exit 1; \
	fi; \
	TOKEN=$$(docker compose run --no-deps --rm -T setup-config \
		/hatchet/hatchet-admin token create \
		--config /hatchet/config \
		--tenant-id "$$TENANT_ID" \
		2>/dev/null | tr -d '\r\n'); \
	if [ -z "$$TOKEN" ]; then \
		echo "Error: token generation failed" >&2; \
		exit 1; \
	fi; \
	printf 'HATCHET_CLIENT_TOKEN=%s\nHATCHET_CLIENT_TLS_STRATEGY=none\n' "$$TOKEN" > $(HATCHET_ENV_FILE); \
	echo "Token written to $(HATCHET_ENV_FILE)"
