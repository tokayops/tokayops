.PHONY: run build test clean swagger up down seed user \
       test-db-start test-db-stop test-db-status \
       test-integration test-integration-quick test-integration-run \
       test-pipeline test-dispatcher \
       e2e-install e2e-test e2e-test-ui e2e-test-headed e2e-up e2e-down \
       webhook-receiver webhook-receiver-build

# Pin swag version for reproducible builds
SWAG_VERSION := v1.16.3

# Build metadata injected into the binary via -ldflags (see cmd/tokayops/main.go).
# Overridable from the environment (e.g. CI): make build GIT_COMMIT=abc123
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
GIT_BRANCH ?= $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.buildBranch=$(GIT_BRANCH) -X main.buildCommit=$(GIT_COMMIT) -X main.buildDate=$(BUILD_DATE)

swagger:
	go run github.com/swaggo/swag/cmd/swag@$(SWAG_VERSION) init -g cmd/tokayops/main.go -o docs

up:
	docker-compose up -d

down:
	docker-compose down

seed:
	go run cmd/tokayops/main.go seed

# Example: make user email=admin@example.com pass=secret name="Admin User"
user:
	go run cmd/tokayops/main.go user create $(email) $(pass) "$(name)"

run: swagger
	go run -ldflags "$(LDFLAGS)" cmd/tokayops/main.go

build: swagger
	go build -ldflags "$(LDFLAGS)" -o tokayops cmd/tokayops/main.go

test:
	go test ./...

clean:
	rm -f tokayops
	rm -f docs/docs.go docs/swagger.json docs/swagger.yaml

reset-db:
	docker-compose down -v

# =============================================================================
# Integration Tests
# =============================================================================
# Test database management
test-db-start:
	@./scripts/test-db.sh start

test-db-stop:
	@./scripts/test-db.sh stop

test-db-status:
	@./scripts/test-db.sh status

# Run all integration tests (auto-starts DB, shows failures only)
test-integration:
	@./scripts/run_integration_tests.sh --failures

# Quick summary of integration tests
test-integration-quick:
	@./scripts/run_integration_tests.sh --summary

# Run specific test pattern (DB must be running)
# Usage: make test-integration-run RUN=TestPipeline_HappyPath
test-integration-run:
	@./scripts/run_integration_tests.sh --no-db --run "$(RUN)"

# Run pipeline tests only
test-pipeline:
	@./scripts/run_integration_tests.sh --run TestPipeline

# Run dispatcher tests only
test-dispatcher:
	@./scripts/run_integration_tests.sh --pkg ./internal/dispatcher/... --failures

# =============================================================================
# E2E Testing
# =============================================================================
e2e-install:
	cd e2e && npm install && npx playwright install chromium firefox

e2e-up:
	docker compose -f docker-compose.e2e.yml up -d --build
	@echo "Waiting for application to be ready..."
	@while ! curl -s http://localhost:8081/swagger/index.html > /dev/null 2>&1; do sleep 2; done
	@echo "Application is ready!"
	docker compose -f docker-compose.e2e.yml exec -T tokay_app /app/tokayops user create admin@example.com 'Admin123!' 'Test Admin' || true
	docker compose -f docker-compose.e2e.yml exec -T tokay_app /app/tokayops seed || true

e2e-down:
	docker compose -f docker-compose.e2e.yml down -v --remove-orphans
	docker network rm tokay_e2e_net 2>/dev/null || true

e2e-test: e2e-up
	@ROOT_DIR=$$(pwd); \
	trap "docker compose -f $$ROOT_DIR/docker-compose.e2e.yml down -v --remove-orphans" EXIT INT TERM; \
	cd e2e && BASE_URL=http://localhost:8081 TEST_USER_EMAIL=admin@example.com TEST_USER_PASSWORD='Admin123!' npx playwright test

e2e-test-ui: e2e-up
	@ROOT_DIR=$$(pwd); \
	trap "docker compose -f $$ROOT_DIR/docker-compose.e2e.yml down -v --remove-orphans" EXIT INT TERM; \
	cd e2e && BASE_URL=http://localhost:8081 TEST_USER_EMAIL=admin@example.com TEST_USER_PASSWORD='Admin123!' npx playwright test --ui

e2e-test-headed: e2e-up
	@ROOT_DIR=$$(pwd); \
	trap "docker compose -f $$ROOT_DIR/docker-compose.e2e.yml down -v --remove-orphans" EXIT INT TERM; \
	cd e2e && BASE_URL=http://localhost:8081 TEST_USER_EMAIL=admin@example.com TEST_USER_PASSWORD='Admin123!' npx playwright test --headed

# =============================================================================
# Webhook Receiver (dev tool)
# =============================================================================
# Usage: make webhook-receiver
# With HMAC: go run cmd/webhook-receiver/main.go --secret mysecret --port 9999
webhook-receiver:
	go run cmd/webhook-receiver/main.go

webhook-receiver-build:
	docker build -f cmd/webhook-receiver/Dockerfile -t webhook-receiver .
