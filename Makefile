# guild — Makefile
#
# Conventions:
#   * `make` with no args prints the self-documented target list.
#   * CGO_ENABLED=0 is baked in — preserves the pure-Go static-binary
#     promise. Override with CGO_ENABLED=1 to experiment.
#   * ldflags mirror .goreleaser.yml so `make install` produces a
#     binary whose `--version` output matches a release artifact.
#   * `make check` is the before-commit gate; `make ci` reproduces
#     the full CI pipeline locally.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

.DEFAULT_GOAL := help

# ----------------------------------------------------------------------
# Variables
# ----------------------------------------------------------------------

GO       ?= go
GOFLAGS  ?= -trimpath
MODULE   := github.com/mathomhaus/guild
BIN_DIR  := bin
BIN      := $(BIN_DIR)/guild
SQLCHECK := $(BIN_DIR)/sqlcheck

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

# Pinned dev-tool versions — match .github/workflows/ci.yml + .goreleaser.yml
# so `make install-tools` produces the same environment CI uses.
GOLANGCI_LINT_VERSION := v1.60.3
GORELEASER_VERSION    := v2.15.0

export CGO_ENABLED ?= 0

# ----------------------------------------------------------------------
# Help (default target)
# ----------------------------------------------------------------------

##@ General

.PHONY: help
help: ## Print this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

# ----------------------------------------------------------------------
# Build
# ----------------------------------------------------------------------

##@ Build

$(BIN_DIR):
	@mkdir -p $(BIN_DIR)

.PHONY: build
build: assets $(BIN_DIR) ## Build the guild binary with embedded assets → bin/guild (ship-ready; requires staged assets)
	$(GO) build $(GOFLAGS) -tags=withembed -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/guild
	@echo "✓ built $(BIN) with -tags=withembed ($(VERSION) $(COMMIT))"

.PHONY: build-fast
build-fast: $(BIN_DIR) ## Build the guild binary WITHOUT embedded assets → bin/guild (dev-iteration only; faster compile, no asset staging needed)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/guild
	@echo "✓ built $(BIN) (no embed, $(VERSION) $(COMMIT))"

.PHONY: build-all
build-all: ## Build all packages (sanity check the full module)
	$(GO) build $(GOFLAGS) ./...

.PHONY: sqlcheck
sqlcheck: $(BIN_DIR) ## Build the SQL-safety analyzer → bin/sqlcheck
	$(GO) build $(GOFLAGS) -o $(SQLCHECK) ./cmd/sqlcheck
	@echo "✓ built $(SQLCHECK)"

.PHONY: install
install: assets ## Install guild to $$GOPATH/bin with embedded assets (ship-ready; requires staged assets via `make assets`)
	$(GO) install $(GOFLAGS) -tags=withembed -ldflags "$(LDFLAGS)" ./cmd/guild
	@echo "✓ installed guild with -tags=withembed ($(VERSION) $(COMMIT))"

.PHONY: install-fast
install-fast: ## Install guild to $$GOPATH/bin WITHOUT embedded assets (dev-iteration only; faster compile, no asset staging needed)
	$(GO) install $(GOFLAGS) -ldflags "$(LDFLAGS)" ./cmd/guild
	@echo "✓ installed guild (no embed, $(VERSION) $(COMMIT))"

.PHONY: install-embed
install-embed: install ## DEPRECATED: use `make install` instead (kept for one cycle so existing scripts do not break)
	@echo "  note: install-embed is deprecated; `make install` now defaults to -tags=withembed"

# ----------------------------------------------------------------------
# Test
# ----------------------------------------------------------------------

##@ Test

.PHONY: test
test: ## Run unit tests (fast, no race detector, no integration)
	$(GO) test -count=1 ./...

.PHONY: test-race
test-race: ## Run unit tests with the race detector (the CI default)
	$(GO) test -race -count=1 ./...

.PHONY: test-short
test-short: ## Run tests with -short (skips slow fixtures)
	$(GO) test -short -count=1 ./...

.PHONY: test-integration
test-integration: build sqlcheck ## Run end-to-end integration tests (builds guild + sqlcheck first)
	$(GO) test -race -count=1 ./tests/integration/...

.PHONY: cover
cover: ## Generate coverage profile → coverage.out
	$(GO) test -race -count=1 -coverprofile=coverage.out -covermode=atomic ./...
	@$(GO) tool cover -func=coverage.out | tail -1

.PHONY: cover-html
cover-html: cover ## Open coverage profile in browser
	$(GO) tool cover -html=coverage.out

# ----------------------------------------------------------------------
# Quality
# ----------------------------------------------------------------------

##@ Quality

.PHONY: fmt
fmt: ## Format Go sources via gofmt
	@gofmt -s -w $(shell find . -name '*.go' -not -path './.git/*' -not -path './.claude/*')
	@echo "✓ gofmt clean"

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## Run golangci-lint (errcheck/govet/staticcheck/gosec/contextcheck/gocritic)
	@command -v golangci-lint >/dev/null || { echo "golangci-lint not installed — run: make install-tools"; exit 1; }
	golangci-lint run --timeout=5m ./...

.PHONY: sqlcheck-run
sqlcheck-run: sqlcheck ## Run the SQL-safety analyzer over the full module
	./$(SQLCHECK) ./...
	@echo "✓ sqlcheck clean"

.PHONY: tidy
tidy: ## Run go mod tidy (fails if it would change go.mod/go.sum)
	$(GO) mod tidy
	@git diff --exit-code go.mod go.sum >/dev/null || { echo "✗ go.mod/go.sum drift — commit tidy changes"; exit 1; }
	@echo "✓ go.mod clean"

.PHONY: check
check: fmt vet lint sqlcheck-run test-race docs-check ## Pre-commit gate: fmt + vet + lint + sqlcheck + test-race + docs drift
	@echo "✓ all pre-commit checks passed"

# ----------------------------------------------------------------------
# Generated docs (cmd/docgen)
# ----------------------------------------------------------------------

##@ Docs

.PHONY: docs
docs: $(BIN_DIR) ## Regenerate docs/generated/* from the live CLI + MCP surfaces
	$(GO) build -trimpath -o bin/docgen ./cmd/docgen
	@HOME=$$(mktemp -d) ./bin/docgen -out docs/generated

.PHONY: docs-check
docs-check: $(BIN_DIR) ## Verify docs/generated/* is up to date (CI gate)
	$(GO) build -trimpath -o bin/docgen ./cmd/docgen
	@HOME=$$(mktemp -d) ./bin/docgen -out docs/generated -check

# ----------------------------------------------------------------------
# Embedded runtime assets (internal/lore/embed/assets/)
# ----------------------------------------------------------------------

##@ Assets

# Source of truth for the model + vocab.
#
# Two paths, in priority order:
#
#   1. ASSETS_SPIKE_DIR (env var). When set AND $(ASSETS_SPIKE_DIR)/model.onnx
#      exists, assets-model copies from there. Preserves the dev workflow
#      against the spike repo and the release.yml staging dir flow.
#
#   2. .model-version + GitHub Release. When ASSETS_SPIKE_DIR is unset or
#      the staged file is missing, assets-model reads .model-version and
#      downloads the pinned model-v<semver> release into a local cache
#      under .cache/model-v<version>/. Subsequent runs hit the cache.
#
# The spike-dir default below stays as a fallback so contributors with a
# local spike checkout keep their fast path; when the directory is absent
# the new download path takes over automatically.
ASSETS_SPIKE_DIR ?= ../lares-spikes/guild-embedding-purego/workspace/models/bge-small-int8

# Pinned model release tag. Bumping this is a deliberate maintainer PR
# (see docs/MODEL.md). The build-model workflow produces the matching
# model-v<MODEL_VERSION> GitHub Release.
MODEL_VERSION := $(shell cat .model-version 2>/dev/null | tr -d '[:space:]')
MODEL_REPO    ?= mathomhaus/guild
MODEL_CACHE   := .cache/model-v$(MODEL_VERSION)

# Per-platform asset root.
ASSETS_DIR := internal/lore/embed/assets

# ONNX Runtime 1.23.x release tag. F2 from the spike friction log:
# onnxruntime-purego pins ORT API v23, which ships in 1.23.x. Bumping
# this requires an ADR because it changes the ABI the purego shim
# assumes.
ORT_VERSION ?= 1.23.0

.PHONY: assets
assets: ## Stage per-platform embedded runtime assets (model, vocab, libonnxruntime)
	@mkdir -p $(ASSETS_DIR)/darwin_arm64 $(ASSETS_DIR)/darwin_amd64 \
	          $(ASSETS_DIR)/linux_amd64  $(ASSETS_DIR)/linux_arm64
	@$(MAKE) assets-model
	@$(MAKE) assets-runtime
	@echo "✓ assets staged under $(ASSETS_DIR)"

.PHONY: assets-model
assets-model: ## Copy model.onnx + vocab.txt into every platform subdir (uses ASSETS_SPIKE_DIR if staged, else downloads pinned model release)
	@src=""; \
	if [ -f "$(ASSETS_SPIKE_DIR)/model.onnx" ] && [ -f "$(ASSETS_SPIKE_DIR)/vocab.txt" ]; then \
	  src="$(ASSETS_SPIKE_DIR)"; \
	  echo "  using staged assets at $$src"; \
	else \
	  if [ -z "$(MODEL_VERSION)" ]; then \
	    echo "✗ no .model-version and ASSETS_SPIKE_DIR is unset / incomplete"; \
	    echo "  fix: either set ASSETS_SPIKE_DIR=<dir-with-model.onnx-and-vocab.txt>"; \
	    echo "       or add a .model-version file at the repo root"; \
	    exit 1; \
	  fi; \
	  if [ ! -f "$(MODEL_CACHE)/model.onnx" ] || [ ! -f "$(MODEL_CACHE)/vocab.txt" ]; then \
	    command -v gh >/dev/null || { echo "✗ gh CLI not found on PATH"; \
	      echo "  fix: install gh (https://cli.github.com) or set ASSETS_SPIKE_DIR to a local staging dir"; exit 1; }; \
	    mkdir -p "$(MODEL_CACHE)"; \
	    echo "  downloading model-v$(MODEL_VERSION) from $(MODEL_REPO) into $(MODEL_CACHE)/"; \
	    gh release download "model-v$(MODEL_VERSION)" \
	      --repo "$(MODEL_REPO)" \
	      --pattern model.onnx \
	      --pattern vocab.txt \
	      --pattern tokenizer.json \
	      --dir "$(MODEL_CACHE)" \
	      || { echo "✗ gh release download failed"; \
	           echo "  fix: run 'gh auth login' or set ASSETS_SPIKE_DIR to a local staging dir"; exit 1; }; \
	  else \
	    echo "  cache hit: $(MODEL_CACHE)"; \
	  fi; \
	  src="$(MODEL_CACHE)"; \
	fi; \
	for triple in darwin_arm64 darwin_amd64 linux_amd64 linux_arm64; do \
	  mkdir -p $(ASSETS_DIR)/$$triple; \
	  cp "$$src/model.onnx" $(ASSETS_DIR)/$$triple/model.onnx; \
	  cp "$$src/vocab.txt"  $(ASSETS_DIR)/$$triple/vocab.txt; \
	done
	@echo "✓ model + vocab staged"

.PHONY: assets-model-clean
assets-model-clean: ## Remove the .cache/model-v* dirs (forces re-download on next assets-model)
	@rm -rf .cache/model-v*
	@echo "✓ model cache cleaned"

.PHONY: assets-runtime
assets-runtime: ## Download libonnxruntime per-platform from the ORT GitHub release
	@command -v curl >/dev/null || { echo "curl not found on PATH"; exit 1; }
	@tmp=$$(mktemp -d); \
	for pair in \
	  "darwin_arm64:osx-arm64:libonnxruntime.$(ORT_VERSION).dylib:libonnxruntime.dylib" \
	  "darwin_amd64:osx-x86_64:libonnxruntime.$(ORT_VERSION).dylib:libonnxruntime.dylib" \
	  "linux_amd64:linux-x64:libonnxruntime.so.$(ORT_VERSION):libonnxruntime.so" \
	  "linux_arm64:linux-aarch64:libonnxruntime.so.$(ORT_VERSION):libonnxruntime.so" ; do \
	    triple=$${pair%%:*}; rest=$${pair#*:}; \
	    ortname=$${rest%%:*}; rest=$${rest#*:}; \
	    libname=$${rest%%:*}; dst=$${rest#*:}; \
	    target=$(ASSETS_DIR)/$$triple/$$dst; \
	    if [ -f $$target ]; then echo "  keep $$target"; continue; fi; \
	    url="https://github.com/microsoft/onnxruntime/releases/download/v$(ORT_VERSION)/onnxruntime-$$ortname-$(ORT_VERSION).tgz"; \
	    tar=$$tmp/$$triple.tgz; \
	    extract=$$tmp/$$triple; \
	    mkdir -p $$extract; \
	    echo "  fetch $$url"; \
	    curl -fsSL -o $$tar $$url; \
	    tar -xzf $$tar -C $$extract; \
	    src=$$(find $$extract -type f -name "$$libname" | head -n1); \
	    if [ -z "$$src" ]; then echo "✗ $$libname not found in $$url"; exit 1; fi; \
	    cp "$$src" "$$target"; \
	    rm -f $$tar; rm -rf $$extract; \
	done; \
	rm -rf $$tmp
	@echo "✓ libonnxruntime staged"

.PHONY: assets-clean
assets-clean: ## Remove every staged asset (keeps directories + README + .gitignore)
	@find $(ASSETS_DIR) -type f \( -name 'model.onnx' -o -name 'vocab.txt' -o -name 'libonnxruntime.*' \) -delete
	@echo "✓ assets cleaned"

.PHONY: regenerate-reference-vectors
regenerate-reference-vectors: assets ## Regenerate internal/lore/embed/testdata/reference_vectors.json against the BUNDLED int8 model
	@$(GO) run -tags=withembed ./cmd/embedref > internal/lore/embed/testdata/reference_vectors.json
	@echo "✓ reference_vectors.json regenerated against bundled int8 model"
	@echo "   (see stderr for provenance: library/model/vocab SHAs, platform, timestamp)"

.PHONY: build-embed
build-embed: build ## DEPRECATED: use `make build` instead (kept for one cycle so existing scripts do not break)
	@echo "  note: build-embed is deprecated; `make build` now defaults to -tags=withembed"

# ----------------------------------------------------------------------
# Release
# ----------------------------------------------------------------------

##@ Release

.PHONY: release-check
release-check: ## Lint .goreleaser.yml config
	@command -v goreleaser >/dev/null || { echo "goreleaser not installed — run: make install-tools"; exit 1; }
	goreleaser check

.PHONY: release-snapshot
release-snapshot: ## Cross-compile all 6 targets locally (no publishing)
	@command -v goreleaser >/dev/null || { echo "goreleaser not installed — run: make install-tools"; exit 1; }
	goreleaser build --snapshot --clean

.PHONY: release-dry-run
release-dry-run: ## Full release dry-run (skip publish; validates signing + Homebrew)
	@command -v goreleaser >/dev/null || { echo "goreleaser not installed — run: make install-tools"; exit 1; }
	goreleaser release --snapshot --clean --skip=publish,sign

# ----------------------------------------------------------------------
# CI (mirrors .github/workflows/*.yml)
# ----------------------------------------------------------------------

##@ CI

.PHONY: ci
ci: ci-build ci-test ci-lint ci-sqlcheck ## Reproduce the full CI pipeline locally
	@echo "✓ CI green locally"

.PHONY: ci-build
ci-build: ## Matrix-style build sanity (CGO=0 go build ./...)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) ./...

.PHONY: ci-test
ci-test: ## CI test job (-race -count=1 ./...)
	$(GO) test -race -count=1 ./...

.PHONY: ci-lint
ci-lint: lint ## CI lint job

.PHONY: ci-sqlcheck
ci-sqlcheck: sqlcheck-run ## CI sqlcheck job

# ----------------------------------------------------------------------
# Dev utilities
# ----------------------------------------------------------------------

##@ Dev

.PHONY: install-tools
install-tools: ## Install pinned dev tools (golangci-lint, goreleaser)
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	$(GO) install github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)
	@echo "✓ dev tools installed — $$($(GO) env GOBIN 2>/dev/null || echo $$($(GO) env GOPATH)/bin)"

.PHONY: run
run: build ## Build and run guild with $$ARGS (e.g. make run ARGS='--help')
	./$(BIN) $(ARGS)

.PHONY: mcp-install
mcp-install: install ## Install guild then register with detected MCP hosts
	guild mcp install

.PHONY: dev
dev: install mcp-install ## Install guild + register with MCP hosts (one-command onboarding)
	@echo "✓ dev ready — guild in \$$PATH, MCP registered"

.PHONY: clean
clean: ## Remove build artifacts
	@rm -rf $(BIN_DIR) dist coverage.out coverage.html
	@echo "✓ cleaned"

# ----------------------------------------------------------------------
# Print key variables (for debugging CI differences)
# ----------------------------------------------------------------------

.PHONY: print-vars
print-vars: ## Print Makefile variables (VERSION, COMMIT, etc.)
	@echo "MODULE  = $(MODULE)"
	@echo "VERSION = $(VERSION)"
	@echo "COMMIT  = $(COMMIT)"
	@echo "DATE    = $(DATE)"
	@echo "LDFLAGS = $(LDFLAGS)"
	@echo "CGO     = $(CGO_ENABLED)"
	@echo "GO      = $(shell $(GO) version)"
