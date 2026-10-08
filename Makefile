COMPOSE  ?= docker compose
APP_PORT ?= 8080
BASE_URL ?= http://localhost:$(APP_PORT)
# Resolved like docker compose does: shell environment first, then ./.env, then the default.
ADMIN_TOKEN      ?= $(or $(shell grep -s '^ADMIN_TOKEN=' .env | cut -d= -f2-),dev-admin-token)
DEMO_INJECT_BUGS ?= $(or $(shell grep -s '^DEMO_INJECT_BUGS=' .env | cut -d= -f2-),true)
# Pinned, not @latest: a new staticcheck release can require a newer Go or add checks, and lint
# must not start failing on its own. v0.7.0 needs Go 1.25, the same as the Docker build image.
STATICCHECK_VERSION ?= v0.7.0

.PHONY: up down logs test lint smoke

up: ## build images and start db, legacy and app in the background
	$(COMPOSE) up -d --build

down: ## stop everything and delete the local database volume
	$(COMPOSE) down -v

logs: ## follow the logs of every service
	$(COMPOSE) logs -f

test: ## unit tests with the race detector
	go test -race -count=1 ./...

lint: ## formatting, vet and staticcheck
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

smoke: ## end-to-end test against BASE_URL (default: the local compose stack)
	BASE_URL="$(BASE_URL)" ADMIN_TOKEN="$(ADMIN_TOKEN)" DEMO_INJECT_BUGS="$(DEMO_INJECT_BUGS)" bash scripts/smoke.sh
