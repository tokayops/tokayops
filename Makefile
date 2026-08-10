.PHONY: run build test clean swagger up down seed user \
       test-db-start test-db-stop test-db-status \
       test-integration test-integration-quick test-integration-run \
       test-pipeline test-dispatcher \
       e2e-install e2e-test e2e-test-ui e2e-test-headed e2e-up e2e-down \
       e2e-wait e2e-seed \
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

# Always from a fresh volume. The environment is built by seeding and then
# resetting, and the reset records a marker that makes it a no-op afterwards -
# so running this twice against a surviving database would re-seed the legacy
# schedules and have nothing left to remove them. Starting clean is also what
# e2e-test already assumed: it tears the stack down after every run.
e2e-up: e2e-down
	docker compose -f docker-compose.e2e.yml up -d --build
	$(MAKE) e2e-wait
	$(MAKE) e2e-seed

# e2e-wait blocks until the app answers, and gives up instead of hanging: an
# unbounded wait is invisible locally (you see it and interrupt it) but in CI it
# burns the whole job timeout and reports nothing about why the app never came
# up. On timeout the app log is the first thing anyone would ask for.
e2e-wait:
	@echo "Waiting for application to be ready..."
	@for attempt in $$(seq 1 60); do \
		if curl -s http://localhost:8081/swagger/index.html > /dev/null 2>&1; then \
			echo "Application is ready!"; \
			exit 0; \
		fi; \
		echo "Waiting for app... (attempt $$attempt/60)"; \
		sleep 2; \
	done; \
	echo "Application failed to start"; \
	docker compose -f docker-compose.e2e.yml logs tokay_app; \
	exit 1

# e2e-seed is the data half of the environment: the admin the suite logs in as,
# the seeded teams, and the destructive reset.
#
# It is a target of its own so that CI calls it instead of keeping a copy of
# these three commands. It used to keep one, and when Epic 10 Sprint 5.5 added
# the reset below, the workflow's copy never got it - so `env.spec.ts` failed on
# every branch in CI while every local run was green. One definition of the
# environment, called from both places, is the only thing that stops that
# happening again.
#
# The reset no longer has anything to delete - `seed` stopped writing schedules
# when the legacy write path was removed - and it stays anyway, without `|| true`,
# so a failure fails the environment. On every run it puts the real CLI against
# the real schema and leaves the marker a database has after an upgrade, which is
# the state the suite is supposed to run against.
#
# Schedules for the tests are created through the API by the Playwright setup
# project.
e2e-seed:
	docker compose -f docker-compose.e2e.yml exec -T tokay_app /app/tokayops user create admin@example.com 'Admin123!' 'Test Admin' || true
	docker compose -f docker-compose.e2e.yml exec -T tokay_app /app/tokayops seed || true
	docker compose -f docker-compose.e2e.yml exec -T tokay_app /app/tokayops migrate reset-schedules

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
