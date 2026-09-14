.DEFAULT_GOAL := help
SHELL := /bin/bash

help: ## show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

init: ## copy .env.example to .env if missing
	@test -f .env || (cp .env.example .env && echo "created .env — edit secrets before running")

up: init ## start all local infrastructure
	docker compose up -d --build

down: ## stop containers, keep volumes
	docker compose down

reset: ## stop containers AND destroy data volumes
	docker compose down -v

logs: ## tail logs for all services
	docker compose logs -f --tail=100

ps: ## show container status
	docker compose ps

psql: ## open a psql shell on the dev database
	docker compose exec postgres psql -U creatorflow -d creatorflow

redis-cli: ## open a redis shell
	docker compose exec redis redis-cli

migrate-up: ## apply all pending migrations
	docker compose exec api migrate -path ./migrations -database "$$DATABASE_URL" up

migrate-down: ## roll back the most recent migration
	docker compose exec api migrate -path ./migrations -database "$$DATABASE_URL" down 1

migrate-new: ## create a migration: make migrate-new name=create_users
	docker compose exec api migrate create -ext sql -dir ./migrations -seq $(name)

test: ## run backend tests
	cd backend && go test ./... -race -count=1

lint: ## run backend linter
	cd backend && golangci-lint run ./...

health: ## hit the API health endpoint
	curl -s localhost:8080/health | jq .

.PHONY: help init up down reset logs ps psql redis-cli migrate-up migrate-down migrate-new test lint health
