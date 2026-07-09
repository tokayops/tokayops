#!/bin/bash
# Test Database Management Script
# Usage:
#   ./scripts/test-db.sh start   - Start test database
#   ./scripts/test-db.sh stop    - Stop test database
#   ./scripts/test-db.sh status  - Check if running
#   ./scripts/test-db.sh logs    - Show database logs

set -e

COMPOSE_PROJECT="tokay_test"
COMPOSE_FILE="docker-compose.test.yml"
CONTAINER_NAME="tokay_test_db_ephemeral"

# Export DSN for other scripts
export TEST_DB_DSN="postgres://test_user:test_password@localhost:5433/test_tokay?sslmode=disable"

case "${1:-status}" in
    start)
        echo "Starting test database..."
        docker-compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" up -d --wait
        echo "Test database started on port 5433"
        echo "DSN: $TEST_DB_DSN"
        ;;
    stop)
        echo "Stopping test database..."
        docker-compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" down
        echo "Test database stopped"
        ;;
    restart)
        $0 stop
        $0 start
        ;;
    status)
        if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
            echo "Test database is RUNNING"
            echo "DSN: $TEST_DB_DSN"
            exit 0
        else
            echo "Test database is NOT RUNNING"
            echo "Run: ./scripts/test-db.sh start"
            exit 1
        fi
        ;;
    logs)
        docker-compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" logs -f
        ;;
    dsn)
        # Just print DSN (for scripting)
        echo "$TEST_DB_DSN"
        ;;
    *)
        echo "Usage: $0 {start|stop|restart|status|logs|dsn}"
        exit 1
        ;;
esac
