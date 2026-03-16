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

# Detect hardware encoders via ffmpeg CLI — independent of our DetectHardwareEncoder() logic.
# This ensures hwtest/qsvtest runs even if our detection has bugs, allowing tests to catch them.
HAS_HW_ENCODER := $(shell ffmpeg -hide_banner -encoders 2>/dev/null | \
	grep -qE '^\s+V..... (hevc|h264)_(qsv|nvenc|vaapi)' && echo "1" || echo "0")
HAS_QSV_ENCODER := $(shell ffmpeg -hide_banner -encoders 2>/dev/null | \
	grep -qE '^\s+V..... (hevc|h264)_qsv' && echo "1" || echo "0")

# Build tags: hwtest when any HW encoder is present; qsvtest when QSV specifically is present.
comma := ,
empty :=
space := $(empty) $(empty)
_test_tags := $(strip $(if $(filter 1,$(HAS_HW_ENCODER)),hwtest )$(if $(filter 1,$(HAS_QSV_ENCODER)),qsvtest))
TEST_TAG_FLAGS := $(if $(_test_tags),-tags $(subst $(space),$(comma),$(_test_tags)))

.PHONY: test
test: fmt vet ## Run tests.
	go test -race -count=1 $(TEST_TAG_FLAGS) ./...

.PHONY: test-integration
test-integration: hatchet-up ## Run integration tests against a local Hatchet server (starts server, generates token).
	env $$(cat $(HATCHET_ENV_FILE)) go test -v -race -count=1 -tags=integration ./...

.PHONY: lint
lint: ## Run golangci-lint.
	golangci-lint run ./...

.PHONY: lint-fix
lint-fix: ## Run golangci-lint and perform fixes.
	golangci-lint run --fix ./...

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
