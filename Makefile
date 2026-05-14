.PHONY: build run test lint tidy migrate-up migrate-down headscale-up headscale-init proto-gen build-control-plane build-flexctl sidecar-image dev-image web-dev web-build e2e e2e-up e2e-down

DB_URL ?= postgres://flex:flex@localhost:5432/flex?sslmode=disable
MIGRATE := go run -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate

build: build-control-plane build-flexctl

web-dev:
	cd web && npm run dev

web-build:
	cd web && npm ci && npm run build

build-control-plane: web-build
	go build -o bin/control-plane ./cmd/control-plane

build-flexctl:
	go build -o bin/flexctl ./cmd/flexctl

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

proto-gen:
	@mkdir -p internal/agentpb
	protoc \
	  --go_out=internal/agentpb --go_opt=paths=source_relative \
	  --go-grpc_out=internal/agentpb --go-grpc_opt=paths=source_relative \
	  --proto_path=proto \
	  agent.proto

sidecar-image:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/flexctl ./cmd/flexctl
	docker build -t flex/sidecar:dev -f images/sidecar/Dockerfile .

dev-image:
	docker build -t flex/dev-cuda-base:dev images/dev-cuda-base

E2E_MIGRATE := $(shell go env GOPATH)/bin/migrate
E2E_DB_URL  := postgres://flex:flex@localhost:5433/flex?sslmode=disable

e2e-up:
	docker compose -f docker-compose.e2e.yml up -d
	@sleep 3
	./scripts/e2e-headscale-init.sh

e2e-down:
	docker compose -f docker-compose.e2e.yml down -v

e2e: build-control-plane e2e-up
	@pkill -f 'bin/control-plane' 2>/dev/null || true
	@sleep 1
	$(E2E_MIGRATE) -path ./migrations -database "$(E2E_DB_URL)" up
	set -a; . ./.env.e2e; set +a; \
	  ./bin/control-plane > /tmp/flex-e2e-cp.log 2>&1 & echo $$! > /tmp/flex-e2e-cp.pid; \
	  sleep 3; \
	  cd web && npx playwright test; rc=$$?; \
	  kill $$(cat /tmp/flex-e2e-cp.pid) 2>/dev/null; exit $$rc
