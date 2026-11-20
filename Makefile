# Load .env if present so `make` targets see the same variables as the services.
-include .env
export

.PHONY: up down fmt vet test api proxy worker gate bootstrap web-install web-dev web-test web-build

up:            ## Start Postgres and wait until it is healthy
	docker compose up -d --wait postgres

down:          ## Stop Postgres (data is kept in the pgdata volume)
	docker compose down

fmt:           ## Fail if any file is not gofmt-formatted
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run gofmt -w ." && exit 1)

vet:
	go vet ./...

test:          ## Unit tests with the race detector (DB tests run when TEST_DATABASE_URL is set)
	go test -race -count=1 ./...

api:           ## Run the API server (applies migrations on start)
	go run ./cmd/api serve

proxy:         ## Run the recorder proxy
	go run ./cmd/proxy

worker:        ## Run a replay worker (set RG_VARIANT and RG_TARGET_URL)
	go run ./cmd/worker

gate:          ## Run the regression gate by hand (needs RG_BASELINE_URL and RG_CANDIDATE_URL)
	go run ./cmd/replaygate run

bootstrap:     ## Create a dev project and print its API key once (make bootstrap NAME=demo OWNER=<github login>)
	go run ./cmd/api bootstrap -name $(or $(NAME),demo) $(if $(OWNER),-owner $(OWNER),)

web-install:   ## Install dashboard dependencies
	cd web && npm ci

web-dev:       ## Dashboard dev server on :5173 (proxies /v1 and /auth to the API on :8080)
	cd web && npm run dev

web-test:      ## Type-check and unit-test the dashboard
	cd web && npm run typecheck && npm test

web-build:     ## Production build into web/dist
	cd web && npm run build
