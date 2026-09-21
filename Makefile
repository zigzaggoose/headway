# Targets are the ones PROJECT.md §5 names, plus the checks CI runs (§11.3) so a
# failure shows up on a laptop before it shows up on a runner.

GO      ?= go
# Load .env for the targets that need credentials, so a laptop run matches what
# Compose gives the container without a dotenv dependency in the binary.
LOADENV  = if [ -f .env ]; then set -a; . ./.env; set +a; fi;
COMPOSE ?= docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.override.yml

.PHONY: help run build migrate test test-integration cover lint fmt vet cross fixtures loadtest up down logs tidy clean

help:
	@grep -hE '^[a-z-]+:.*?##' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  %-18s %s\n", $$1, $$2}'

run: ## Run the service against the local database. Reads .env if present.
	@$(LOADENV) $(GO) run ./cmd/headway

build: ## Build for this machine.
	$(GO) build -o bin/headway ./cmd/headway

migrate: ## Apply migrations and exit. The service also migrates on startup.
	@$(LOADENV) $(GO) run ./cmd/headway -migrate-only

test: ## Unit tests, race detector on, no cache.
	$(GO) test -race -count=1 ./...

test-integration: ## Unit plus integration tests. Needs DATABASE_URL_TEST.
	$(GO) test -race -tags=integration -count=1 ./...

cover: ## Print total coverage. No gate in v1 (§11.3).
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

lint: vet ## gofmt and go vet. staticcheck joins at Stage 2.
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

fmt: ## Rewrite files with gofmt.
	gofmt -w .

vet:
	$(GO) vet ./...

cross: ## The deployment build. Catches the arm64 mistake CI checks for.
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build -o /dev/null ./cmd/headway

fixtures: ## Capture a live feed response into testdata/. Needs TFNSW_API_KEY.
	@$(LOADENV) $(GO) run ./cmd/fixturedump

loadtest: ## Load test per docs/loadtest.md. Stage 3.
	@echo "see docs/loadtest.md"

up: ## Postgres and the service, locally.
	$(COMPOSE) up --build -d

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f headway

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin coverage.out
