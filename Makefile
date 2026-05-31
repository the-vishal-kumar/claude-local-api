# Makefile for claude-local-api.
# Run `make help` to list targets.

BINARY := claude-local-api
PKG    := ./...
CMD    := ./src/cmd/$(BINARY)
IMAGE  := claude-local-api:latest

.PHONY: help all build run test fmt vet tidy clean \
        env start stop logs token login docker-build

help: ## Show this help.
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

all: fmt vet test build ## Format, vet, test, then build.

build: ## Compile the binary into ./bin.
	go build -o bin/$(BINARY) $(CMD)

run: ## Build and start the server.
	go run $(CMD)

test: ## Run all tests with the race detector.
	go test -race $(PKG)

fmt: ## Format all Go source.
	gofmt -w .

vet: ## Run go vet.
	go vet $(PKG)

tidy: ## Tidy go.mod.
	go mod tidy

clean: ## Remove build artifacts.
	rm -rf bin

# ── Docker (run the service in a container) ──────────────────────────────────
# First time:   make token   (one-time auth)   then   make start
# After that:   make start    /    make stop    /    make logs

env: ## Create .env from .env.example if it doesn't exist.
	@[ -f .env ] && echo ".env already exists" || (cp .env.example .env && echo "created .env")

start: env ## Build if needed and start the service (waits for health, prints the URL).
	docker compose up -d --build
	@curl --retry 30 --retry-connrefused --retry-delay 1 -fsS localhost:8787/healthz >/dev/null 2>&1 \
		&& echo "Ready → http://localhost:8787/docs   (if /v1/run returns an auth error, run: make token)" \
		|| echo "Started, but the health check timed out — check 'make logs'."

token: env ## One-time: mint a subscription token and save it into .env for you.
	docker compose run --rm -it claude-local-api claude setup-token
	@printf 'Paste the token shown above, then press Enter:\n> '; \
		read tok; \
		[ -n "$$tok" ] || { echo "No token entered; .env unchanged."; exit 1; }; \
		tmp=$$(mktemp); grep -v '^CLAUDE_CODE_OAUTH_TOKEN=' .env > "$$tmp" 2>/dev/null || true; mv "$$tmp" .env; \
		printf 'CLAUDE_CODE_OAUTH_TOKEN=%s\n' "$$tok" >> .env; \
		echo "Saved CLAUDE_CODE_OAUTH_TOKEN to .env — now run 'make start'."

login: ## Alternative auth: interactive /login, persisted in the claude-home volume.
	docker compose run --rm -it claude-local-api claude

stop: ## Stop the service.
	docker compose down

logs: ## Follow the service logs.
	docker compose logs -f

docker-build: ## Build the container image only (start does this for you).
	docker compose build
