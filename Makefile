# shithub Makefile
# Targets mirror what CI runs. The Makefile is the source of truth.

.DEFAULT_GOAL := help
.PHONY: help dev build test test-race lint fmt tidy clean ci assets install-tools version

# Build metadata embedded into the binary via -ldflags.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILT   := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/tenseleyFlow/shithub/internal/version.Version=$(VERSION) \
           -X github.com/tenseleyFlow/shithub/internal/version.Commit=$(COMMIT) \
           -X github.com/tenseleyFlow/shithub/internal/version.BuiltAt=$(BUILT)

GOFLAGS := -trimpath
BIN     := bin/shithubd

# Tools installed via 'go install' live in GOBIN (or GOPATH/bin). Reference
# them by absolute path so make recipes don't depend on PATH ordering.
GOBIN := $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN := $(shell go env GOPATH)/bin
endif
GOFUMPT   := $(GOBIN)/gofumpt
GOIMPORTS := $(GOBIN)/goimports
AIR       := $(GOBIN)/air

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

dev: ## Run the web server with hot reload via air.
	@$(AIR)

build: ## Build the shithubd binary into bin/.
	@mkdir -p bin
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/shithubd

test: ## Run unit tests.
	go test $(GOFLAGS) ./...

test-race: ## Run unit tests with the race detector.
	go test $(GOFLAGS) -race ./...

lint: ## Run golangci-lint.
	golangci-lint run

fmt: ## Format the codebase with gofumpt and goimports.
	$(GOFUMPT) -l -w cmd internal pkg
	$(GOIMPORTS) -local github.com/tenseleyFlow/shithub -w cmd internal pkg

tidy: ## Tidy go.mod / go.sum.
	go mod tidy

clean: ## Remove build artifacts.
	rm -rf bin tmp coverage.out

assets: ## Copy Primer CSS into internal/web/static/ for embedding.
	@mkdir -p internal/web/static/primer
	@if [ -d .refs/primer-css/dist ]; then \
		cp -R .refs/primer-css/dist/* internal/web/static/primer/; \
	else \
		echo "warn: .refs/primer-css/dist not found; run 'git clone https://github.com/primer/css .refs/primer-css' first"; \
	fi

ci: lint test build ## Full CI pipeline (matches .github/workflows/ci.yml).
	@echo "ci: ok"

install-tools: ## Install development tools via 'go install'.
	go install mvdan.cc/gofumpt@latest
	go install golang.org/x/tools/cmd/goimports@latest
	go install github.com/air-verse/air@latest

version: ## Print version info that would be embedded into the binary.
	@echo "Version: $(VERSION)"
	@echo "Commit:  $(COMMIT)"
	@echo "Built:   $(BUILT)"
