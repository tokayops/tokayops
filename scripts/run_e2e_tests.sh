#!/bin/bash
set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${YELLOW}Starting E2E test environment...${NC}"

# Cleanup function
cleanup() {
    echo -e "${YELLOW}Cleaning up...${NC}"
    docker compose -f docker-compose.e2e.yml down -v --remove-orphans
}

# Set trap for cleanup on exit
trap cleanup EXIT

# Start the E2E environment
docker compose -f docker-compose.e2e.yml up -d --build

# Wait for the app to be healthy
echo -e "${YELLOW}Waiting for application to be ready...${NC}"
MAX_ATTEMPTS=60
ATTEMPT=0

while [ $ATTEMPT -lt $MAX_ATTEMPTS ]; do
    if curl -s http://localhost:8081/swagger/index.html > /dev/null 2>&1; then
        echo -e "${GREEN}Application is ready!${NC}"
        break
    fi
    ATTEMPT=$((ATTEMPT + 1))
    echo "Waiting for app... (attempt $ATTEMPT/$MAX_ATTEMPTS)"
    sleep 2
done

if [ $ATTEMPT -eq $MAX_ATTEMPTS ]; then
    echo -e "${RED}Application failed to start${NC}"
    docker compose -f docker-compose.e2e.yml logs tokay_app
    exit 1
fi

# Create test user
echo -e "${YELLOW}Creating test user...${NC}"
docker compose -f docker-compose.e2e.yml exec -T tokay_app /app/tokayops user create admin@example.com 'Admin123!' 'Test Admin' || true

# Seed test data
echo -e "${YELLOW}Seeding test data...${NC}"
docker compose -f docker-compose.e2e.yml exec -T tokay_app /app/tokayops seed || true

# Run Playwright tests
echo -e "${YELLOW}Running E2E tests...${NC}"
cd e2e

# Install dependencies if needed
if [ ! -d "node_modules" ]; then
    echo -e "${YELLOW}Installing dependencies...${NC}"
    npm ci
    npx playwright install chromium firefox
fi

# Set environment variables for tests
export BASE_URL=http://localhost:8081
export TEST_USER_EMAIL=admin@example.com
export TEST_USER_PASSWORD='Admin123!'

# Run tests
if [ "$1" == "--ui" ]; then
    npx playwright test --ui
elif [ "$1" == "--headed" ]; then
    npx playwright test --headed
elif [ "$1" == "--debug" ]; then
    npx playwright test --debug
else
    npx playwright test "$@"
fi

TEST_EXIT_CODE=$?

cd ..

if [ $TEST_EXIT_CODE -eq 0 ]; then
    echo -e "${GREEN}E2E tests passed!${NC}"
else
    echo -e "${RED}E2E tests failed!${NC}"
fi

exit $TEST_EXIT_CODE
