.PHONY: help up down ollama-up ollama-down ollama-logs logs build run test test-race test-integration test-ollama test-docker vet fmt check

help:
	@echo "up               Start all services with Docker Compose"
	@echo "down             Stop services; keep database volume"
	@echo "ollama-up        Start app, database and Ollama; prepare the local model"
	@echo "ollama-down      Stop the Ollama stack; keep data and downloaded models"
	@echo "ollama-logs      Follow local Ollama and application logs"
	@echo "logs             Follow application logs"
	@echo "build            Build bin/expenses"
	@echo "run              Run locally (requires PostgreSQL)"
	@echo "test             Run tests; DB tests need TEST_DATABASE_URL"
	@echo "test-race        Run tests with Go race detector"
	@echo "test-integration Run DB tests against TEST_DATABASE_URL"
	@echo "test-ollama      Run opt-in acceptance tests against a real local Ollama and test DB"
	@echo "test-docker      Run all tests with an isolated PostgreSQL in Docker"
	@echo "check            Run tests and go vet"

up:
	docker compose up --build -d

down:
	docker compose down

ollama-up:
	docker compose -f compose.yaml -f compose.ollama.yaml up --build -d

ollama-down:
	docker compose -f compose.yaml -f compose.ollama.yaml down

ollama-logs:
	docker compose -f compose.yaml -f compose.ollama.yaml logs -f app ollama ollama-model

logs:
	docker compose logs -f app

build:
	go build -trimpath -o bin/expenses ./cmd/server

run:
	go run ./cmd/server

test:
	go test ./...

test-race:
	go test -race ./...

test-docker:
	docker compose -f compose.test.yaml up --build --abort-on-container-exit --exit-code-from tests

test-integration:
	@test -n "$$TEST_DATABASE_URL" || (echo "Set TEST_DATABASE_URL to a dedicated test database"; exit 1)
	go test -count=1 ./internal/expenses ./internal/httpapi ./internal/bot

test-ollama:
	@test -n "$$TEST_DATABASE_URL" || (echo "Set TEST_DATABASE_URL to a dedicated test database"; exit 1)
	@test -n "$$TEST_OLLAMA_URL" || (echo "Set TEST_OLLAMA_URL, for example http://localhost:11434/v1"; exit 1)
	go test -count=1 -timeout=10m -run TestLiveOllama -v ./internal/bot

vet:
	go vet ./...

fmt:
	go fmt ./...

check: test vet
