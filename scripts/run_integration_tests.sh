#!/bin/bash
# Integration Test Runner
#
# Usage:
#   ./scripts/run_integration_tests.sh              - Run all tests (auto-start DB)
#   ./scripts/run_integration_tests.sh --no-db      - Run tests (DB must be running)
#   ./scripts/run_integration_tests.sh --run NAME   - Run specific test(s) matching NAME
#   ./scripts/run_integration_tests.sh --pkg PKG    - Run tests in specific package
#   ./scripts/run_integration_tests.sh --summary    - Show only pass/fail summary
#   ./scripts/run_integration_tests.sh --failures   - Show only failures (default for CI)
#
# Examples:
#   ./scripts/run_integration_tests.sh --run TestPipeline
#   ./scripts/run_integration_tests.sh --run TestPipeline_HappyPath
#   ./scripts/run_integration_tests.sh --pkg ./internal/integration/...
#   ./scripts/run_integration_tests.sh --no-db --run TestPipeline --failures

set -e

# Defaults
AUTO_DB=true
RUN_PATTERN=""
PACKAGE="./internal/..."
OUTPUT_MODE="full"  # full, summary, failures
VERBOSE="-v"

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --no-db)
            AUTO_DB=false
            shift
            ;;
        --run)
            RUN_PATTERN="$2"
            shift 2
            ;;
        --pkg)
            PACKAGE="$2"
            shift 2
            ;;
        --summary)
            OUTPUT_MODE="summary"
            VERBOSE=""
            shift
            ;;
        --failures)
            OUTPUT_MODE="failures"
            shift
            ;;
        --quiet|-q)
            VERBOSE=""
            shift
            ;;
        --help|-h)
            head -20 "$0" | tail -19
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            echo "Use --help for usage"
            exit 1
            ;;
    esac
done

# Set DSN
export TEST_DB_DSN="postgres://test_user:test_password@localhost:5433/test_tokay?sslmode=disable"

# Start DB if needed
if $AUTO_DB; then
    echo "Starting test database..."
    docker-compose -p tokay_test -f docker-compose.test.yml up -d --wait
    trap 'docker-compose -p tokay_test -f docker-compose.test.yml down' EXIT
else
    # Check if DB is running
    if ! docker ps --format '{{.Names}}' | grep -q "^tokay_test_db_ephemeral$"; then
        echo "ERROR: Test database is not running"
        echo "Start it with: ./scripts/test-db.sh start"
        exit 1
    fi
fi

# Build test command
TEST_CMD="go test -p 1 -tags=integration $VERBOSE"
if [ -n "$RUN_PATTERN" ]; then
    TEST_CMD="$TEST_CMD -run $RUN_PATTERN"
fi
TEST_CMD="$TEST_CMD $PACKAGE"

echo "Running: $TEST_CMD"
echo "---"

# Run tests based on output mode
case $OUTPUT_MODE in
    summary)
        # Only show final pass/fail counts
        if $TEST_CMD 2>&1 | tee /tmp/test_output.txt | grep -E "^(ok|FAIL|\?)" ; then
            echo "---"
            echo "PASSED"
        else
            echo "---"
            echo "FAILED - see /tmp/test_output.txt for details"
            exit 1
        fi
        ;;
    failures)
        # Run and filter to show only failures
        set +e
        $TEST_CMD 2>&1 | tee /tmp/test_output.txt
        TEST_EXIT=${PIPESTATUS[0]}
        set -e

        if [ $TEST_EXIT -ne 0 ]; then
            echo ""
            echo "=== FAILURES SUMMARY ==="
            grep -E "(--- FAIL|FAIL\t|Error|panic:)" /tmp/test_output.txt || true
            echo ""
            echo "Full output: /tmp/test_output.txt"
            exit 1
        else
            echo ""
            echo "=== ALL TESTS PASSED ==="
            grep -E "^ok\s" /tmp/test_output.txt | wc -l | xargs echo "Packages passed:"
        fi
        ;;
    full)
        # Full verbose output
        $TEST_CMD
        echo ""
        echo "=== ALL TESTS PASSED ==="
        ;;
esac
