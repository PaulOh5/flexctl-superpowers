.PHONY: build run test lint tidy migrate-up migrate-down

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
