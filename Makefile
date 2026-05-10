.PHONY: build run test lint tidy migrate-up migrate-down headscale-up headscale-init

DB_URL ?= postgres://flex:flex@localhost:5432/flex?sslmode=disable
MIGRATE := go run -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate

build:
	go build -o bin/control-plane ./cmd/control-plane

run:
	go run ./cmd/control-plane

test:
	go test ./... -race -count=1

lint:
	go vet ./...

tidy:
	go mod tidy

migrate-up:
	$(MIGRATE) -path ./migrations -database "$(DB_URL)" up

migrate-down:
	$(MIGRATE) -path ./migrations -database "$(DB_URL)" down 1

headscale-up:
	docker compose up -d headscale
	@echo "waiting for headscale to be healthy..."
	@n=0; until curl -fsS http://localhost:8088/health >/dev/null 2>&1; do \
	  n=$$((n+1)); if [ $$n -ge 30 ]; then echo "headscale did not become healthy in 30s"; exit 1; fi; \
	  sleep 1; done
	@echo "headscale is up"

headscale-init:
	@echo "creating headscale 'control-plane' user (idempotent — ignore 'already exists' error)..."
	@docker compose exec -T headscale headscale users create control-plane 2>/dev/null || true
	@echo "creating API key (1y expiration); save the printed value as FLEX_HEADSCALE_API_KEY:"
	@docker compose exec -T headscale headscale apikeys create --expiration 365d
