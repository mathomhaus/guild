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
build: $(BIN_DIR) ## Build the guild binary → bin/guild
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/guild
	@echo "✓ built $(BIN) ($(VERSION) $(COMMIT))"

.PHONY: build-all
build-all: ## Build all packages (sanity check the full module)
	$(GO) build $(GOFLAGS) ./...

.PHONY: sqlcheck
sqlcheck: $(BIN_DIR) ## Build the SQL-safety analyzer → bin/sqlcheck
	$(GO) build $(GOFLAGS) -o $(SQLCHECK) ./cmd/sqlcheck
	@echo "✓ built $(SQLCHECK)"

.PHONY: install
install: ## Install guild to $$GOPATH/bin (go install with ldflags)
	$(GO) install $(GOFLAGS) -ldflags "$(LDFLAGS)" ./cmd/guild
	@echo "✓ installed guild ($(VERSION) $(COMMIT))"

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

.PHONY: test-concurrency
test-concurrency: ## Run the multi-agent concurrency stress suite (-race) — QUEST-179
	$(GO) test -race -count=1 -v ./internal/test/concurrency/...

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
