# shithub Makefile
# Targets mirror what CI runs. The Makefile is the source of truth.

.DEFAULT_GOAL := help
.PHONY: help dev build test test-race lint lint-policy lint-markdown lint-secret-logs lint-spdx lint-unused lint-migrations verify-api-docs fmt tidy clean ci assets install-tools version deploy deploy-check restore-drill bench-staging docs docs-serve docs-verify gen-third-party-notices audit-a11y audit-a11y-pa11y audit-a11y-axe load-test

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

dev: ## Run the web server with hot reload via air. Sources .env if present.
	@if [ -f .env ]; then set -a; . ./.env; set +a; fi; $(AIR)

dev-migrate: ## Apply DB migrations against $$SHITHUB_DATABASE_URL (sources .env).
	@if [ -f .env ]; then set -a; . ./.env; set +a; fi; \
	go run ./cmd/shithubd migrate up

dev-run: ## Run the binary directly (no air); sources .env.
	@if [ -f .env ]; then set -a; . ./.env; set +a; fi; \
	go run ./cmd/shithubd web

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

ci: lint lint-policy lint-markdown lint-secret-logs lint-spdx lint-unused lint-migrations verify-api-docs test build ## Full CI pipeline (matches .github/workflows/ci.yml).
	@echo "ci: ok"

lint-policy: ## Enforce policy-package boundary (no inline auth checks in handlers/git/cmd).
	@scripts/lint-policy-boundary.sh

lint-markdown: ## Enforce markdown-package boundary (no goldmark/bluemonday outside internal/markdown).
	@scripts/lint-markdown-boundary.sh

lint-secret-logs: ## Fail when source emits log lines containing token-prefix patterns.
	@scripts/lint-secret-logs.sh

lint-spdx: ## Verify every Go + shell source carries the SPDX license header.
	@scripts/verify-spdx-headers.sh

lint-unused: ## Fail when source carries dead-code 'silence unused import' shims (var _ = symbol).
	@scripts/lint-unused.sh

lint-migrations: ## Fail when goose migration numeric versions collide.
	@scripts/lint-migration-versions.sh

verify-api-docs: ## Fail when an /api/v1 route in code is missing from docs/public/api/.
	@scripts/verify-api-docs.sh

bench-small: ## Run the bench harness against $$BENCH_TARGET (default localhost:8080).
	@go run ./bench -target=$${BENCH_TARGET:-http://localhost:8080} -iters=$${BENCH_ITERS:-20}

bench-full: ## Placeholder — runs nightly off-CI against big fixtures (see bench/fixtures/README.md).
	@echo "bench-full: big-fixture generators land in a follow-up — see bench/fixtures/README.md"
	@exit 0

install-tools: ## Install development tools via 'go install'.
	go install mvdan.cc/gofumpt@latest
	go install golang.org/x/tools/cmd/goimports@latest
	go install github.com/air-verse/air@latest
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest

dev-db: ## Bring up Postgres in docker-compose.
	docker compose up -d postgres
	@echo "Waiting for postgres to become healthy..."
	@until docker compose exec -T postgres pg_isready -U shithub -d shithub >/dev/null 2>&1; do sleep 1; done
	@echo "Postgres ready at 127.0.0.1:5432"

dev-db-down: ## Stop the dev Postgres container.
	docker compose down

dev-db-reset: ## Drop the dev Postgres volume and re-create.
	docker compose down -v
	$(MAKE) dev-db

dev-storage: ## Bring up MinIO + run minio-init to seed the bucket.
	docker compose up -d minio
	docker compose run --rm minio-init
	@echo "MinIO S3 API: http://127.0.0.1:9000  console: http://127.0.0.1:9001"
	@echo "Credentials: shithub-dev / shithub-dev-secret-please-change"

dev-storage-down: ## Stop the MinIO container (volume persists).
	docker compose stop minio

dev-storage-reset: ## Drop the MinIO volume and re-seed.
	docker compose down minio
	docker volume rm -f shithub-miniodata
	$(MAKE) dev-storage

storage-check: build ## Run shithubd storage check against the configured backend.
	./bin/shithubd storage check

dev-email: ## Bring up MailHog for local email capture (S05).
	docker compose up -d mailhog
	@echo "MailHog SMTP: 127.0.0.1:1025  web UI: http://127.0.0.1:8025"

dev-email-down: ## Stop MailHog.
	docker compose stop mailhog

migrate-up: ## Apply all pending migrations.
	./bin/shithubd migrate up

migrate-down: ## Roll back the most recent migration.
	./bin/shithubd migrate down

migrate-status: ## Show migration status.
	./bin/shithubd migrate status

sqlc-generate: ## Regenerate sqlc Go code from queries.
	$(GOBIN)/sqlc generate

test-integration: ## Run tests with SHITHUB_TEST_DATABASE_URL set against the dev Postgres.
	SHITHUB_TEST_DATABASE_URL=$${SHITHUB_TEST_DATABASE_URL:-postgres://shithub:shithub_dev@127.0.0.1:5432/postgres?sslmode=disable} \
		go test -trimpath ./...

version: ## Print version info that would be embedded into the binary.
	@echo "Version: $(VERSION)"
	@echo "Commit:  $(COMMIT)"
	@echo "Built:   $(BUILT)"

# --- deploy ---
# Inventory selection: ANSIBLE_INVENTORY=production make deploy (default: staging).
ANSIBLE_INVENTORY ?= staging
ANSIBLE_TAGS      ?=
ANSIBLE_LIMIT     ?=

deploy-check: ## Dry-run the Ansible playbook (--check) against $$ANSIBLE_INVENTORY.
	cd deploy/ansible && ansible-playbook -i inventory/$(ANSIBLE_INVENTORY) site.yml --check --diff \
		$(if $(ANSIBLE_TAGS),--tags $(ANSIBLE_TAGS)) \
		$(if $(ANSIBLE_LIMIT),--limit $(ANSIBLE_LIMIT))

deploy: ## Apply the Ansible playbook against $$ANSIBLE_INVENTORY (set to production for prod).
	cd deploy/ansible && ansible-playbook -i inventory/$(ANSIBLE_INVENTORY) site.yml \
		$(if $(ANSIBLE_TAGS),--tags $(ANSIBLE_TAGS)) \
		$(if $(ANSIBLE_LIMIT),--limit $(ANSIBLE_LIMIT))

restore-drill: ## Run the restore drill on the backup host (must be run via ssh on that host).
	deploy/restore-drill/run.sh

bench-staging: ## Run the bench harness against staging (BENCH_TARGET must be set to the staging URL).
	@if [ -z "$$BENCH_TARGET" ]; then echo "set BENCH_TARGET=https://staging.shithub.example"; exit 2; fi
	go run ./bench -target=$$BENCH_TARGET -iters=$${BENCH_ITERS:-50}

# --- docs ---
docs: ## Build the public docs site to build/docs/ via mdBook.
	cd docs/public && mdbook build

docs-serve: ## Serve the public docs site locally on http://127.0.0.1:3000.
	cd docs/public && mdbook serve --port 3000

docs-verify: verify-api-docs ## Verify docs are in sync (API routes documented + SPDX headers).
	@$(MAKE) lint-spdx
	@if command -v mdbook >/dev/null 2>&1; then \
		cd docs/public && mdbook build >/dev/null && echo "mdbook build: ok"; \
	else \
		echo "mdbook not installed; skipping site build"; \
	fi

gen-third-party-notices: ## Regenerate THIRD_PARTY_NOTICES.md from the active go.mod.
	@scripts/gen-third-party-notices.sh > THIRD_PARTY_NOTICES.md
	@echo "gen-third-party-notices: wrote THIRD_PARTY_NOTICES.md"

# --- S39 hardening ---
audit-a11y-pa11y: ## pa11y-ci scan of anonymous routes (needs running shithub on 127.0.0.1:8080).
	@command -v pa11y-ci >/dev/null 2>&1 || { echo "pa11y-ci not installed; npm i -g pa11y-ci"; exit 2; }
	pa11y-ci --config tests/a11y/pa11y-config.json

audit-a11y-axe: ## axe-core scan of authenticated routes (needs SHITHUB_USER + SHITHUB_PASS).
	@command -v node >/dev/null 2>&1 || { echo "node not installed"; exit 2; }
	node tests/a11y/axe-runner.js

audit-a11y: audit-a11y-pa11y audit-a11y-axe ## Run both accessibility scans.

load-test: ## Run a k6 scenario (set K6_SCENARIO=mixed-read|auth-mix|issue-comment-storm|search-load; default mixed-read).
	@command -v k6 >/dev/null 2>&1 || { echo "k6 not installed; see https://k6.io/docs/getting-started/installation/"; exit 2; }
	@if [ -z "$$BASE" ] && [ -z "$$BENCH_TARGET" ]; then echo "set BASE or BENCH_TARGET (e.g. https://staging.shithub.example)"; exit 2; fi
	BASE="$${BASE:-$$BENCH_TARGET}" k6 run tests/load/k6/scenarios/$${K6_SCENARIO:-mixed-read}.js
