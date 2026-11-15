SHIPYARD_DATABASE_URL ?= postgres://shipyard:shipyard-dev@localhost:5432/shipyard?sslmode=disable
export SHIPYARD_DATABASE_URL

.PHONY: dev build test test-integration lint fmt migrate sqlc

dev:
	docker compose up -d --build
	cd web && npm ci && npm run dev

build:
	go build -o bin/shipyard ./cmd/shipyard
	cd web && npm ci && npm run build

test:
	go test ./...

test-integration:
	go test -tags=integration -count=1 ./...

lint:
	golangci-lint run ./...
	cd web && npm run lint

fmt:
	go fmt ./...

migrate:
	go run ./cmd/shipyard migrate

sqlc:
	sqlc generate
