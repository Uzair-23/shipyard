SHIPYARD_DATABASE_URL ?= postgres://shipyard:shipyard-dev@localhost:5434/shipyard?sslmode=disable
export SHIPYARD_DATABASE_URL

.PHONY: dev build test test-integration test-security lint fmt migrate sqlc worker

dev:
	docker compose up -d --build
	cd web && npm ci && npm run dev

build:
	go build -o bin/shipyard ./cmd/shipyard
	cd web && npm ci && npm run build

test:
	go test ./...

# -p 1: starting testcontainers from several packages at once is flaky on Docker Desktop.
test-integration:
	go test -tags=integration -count=1 -p 1 ./...

test-security:
	go test -count=1 ./security/...

lint:
	golangci-lint run ./...
	cd web && npm run lint

fmt:
	go fmt ./...

migrate:
	go run ./cmd/shipyard migrate

# M1: the worker runs on the host (ADR 011); it moves into compose in M2.
worker:
	go run ./cmd/shipyard worker

sqlc:
	sqlc generate
