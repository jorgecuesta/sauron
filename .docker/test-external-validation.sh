#!/bin/bash

# Don't exit on error - we want to see what fails
# set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Test counters
TESTS_RUN=0
TESTS_PASSED=0
TESTS_FAILED=0

print_header() {
    echo -e "\n${BLUE}========================================${NC}"
    echo -e "${BLUE}$1${NC}"
    echo -e "${BLUE}========================================${NC}\n"
}

print_test() {
    echo -e "${YELLOW}TEST $((TESTS_RUN + 1)):${NC} $1"
}

print_pass() {
    echo -e "${GREEN}✓ PASS:${NC} $1"
    ((TESTS_PASSED++))
}

print_fail() {
    echo -e "${RED}✗ FAIL:${NC} $1"
    ((TESTS_FAILED++))
}

print_info() {
    echo -e "${BLUE}ℹ INFO:${NC} $1"
}

run_test() {
    ((TESTS_RUN++))
}

# Cleanup function
cleanup() {
    print_header "Cleaning Up"
    docker-compose down -v 2>/dev/null || true
    echo "Cleanup complete"
}

# Set trap for cleanup on exit
trap cleanup EXIT

print_header "External Endpoint Validation Test Suite"
echo "This script tests the external endpoint discovery and validation features"
echo "of Sauron's distributed architecture."
echo ""
echo "Architecture:"
echo "  Primary Ring   -> Monitors secondary ring as external endpoint"
echo "  Secondary Ring -> Acts as leaf node (no external rings)"
echo ""

# Test 1: Start Docker Compose
print_header "Test 1: Starting Docker Compose Environment"
run_test
print_test "Starting containers with docker-compose"

# First ensure images are built
print_info "Building Docker images (this may take a few minutes on first run)..."
if ! docker-compose build 2>&1 | tail -5; then
    print_fail "Failed to build Docker images"
    exit 1
fi

if docker-compose up -d 2>&1 | tail -10; then
    print_pass "Docker Compose started successfully"
else
    print_fail "Failed to start Docker Compose"
    exit 1
fi

# Test 2: Wait for containers to be healthy
print_header "Test 2: Waiting for Containers to be Healthy"
run_test
print_test "Waiting for secondary container to be healthy (max 30s)"

TIMEOUT=30
ELAPSED=0
while [ $ELAPSED -lt $TIMEOUT ]; do
    HEALTH=$(docker inspect --format='{{.State.Health.Status}}' sauron-secondary 2>/dev/null || echo "not_found")
    if [ "$HEALTH" = "healthy" ]; then
        print_pass "Secondary container is healthy after ${ELAPSED}s"
        break
    fi
    sleep 1
    ((ELAPSED++))
done

if [ "$HEALTH" != "healthy" ]; then
    print_fail "Secondary container failed to become healthy after ${TIMEOUT}s"
    docker logs sauron-secondary --tail 20
    exit 1
fi

run_test
print_test "Waiting for primary container to be healthy (max 30s)"

TIMEOUT=30
ELAPSED=0
while [ $ELAPSED -lt $TIMEOUT ]; do
    HEALTH=$(docker inspect --format='{{.State.Health.Status}}' sauron-primary 2>/dev/null || echo "not_found")
    if [ "$HEALTH" = "healthy" ]; then
        print_pass "Primary container is healthy after ${ELAPSED}s"
        break
    fi
    sleep 1
    ((ELAPSED++))
done

if [ "$HEALTH" != "healthy" ]; then
    print_fail "Primary container failed to become healthy after ${TIMEOUT}s"
    docker logs sauron-primary --tail 20
    exit 1
fi

# Test 3: Verify containers are running
print_header "Test 3: Verifying Container Status"
run_test
print_test "Checking running containers"

echo ""
docker ps --filter "name=sauron" --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"
echo ""

RUNNING=$(docker ps --filter "name=sauron" --filter "status=running" -q | wc -l)
if [ "$RUNNING" -eq 2 ]; then
    print_pass "Both containers are running"
else
    print_fail "Expected 2 containers running, found $RUNNING"
fi

# Test 4: Wait for external endpoint discovery
print_header "Test 4: External Endpoint Discovery"
run_test
print_test "Waiting for primary to discover secondary's endpoints (max 15s)"

sleep 15  # Give it time to run the external checker

print_info "Checking primary logs for endpoint discovery..."
DISCOVERY_LOGS=$(docker logs sauron-primary 2>&1 | grep "Stored new advertised endpoint" | wc -l)

if [ "$DISCOVERY_LOGS" -ge 3 ]; then
    print_pass "Primary discovered endpoints from secondary (found $DISCOVERY_LOGS entries)"
    echo ""
    docker logs sauron-primary 2>&1 | grep "Stored new advertised endpoint"
    echo ""
else
    print_fail "Primary did not discover expected endpoints (found $DISCOVERY_LOGS entries, expected >= 3)"
fi

# Test 5: Verify endpoint validation
print_header "Test 5: Endpoint Validation"
run_test
print_test "Checking if endpoints were validated successfully"

print_info "Checking primary logs for successful validation..."
VALIDATION_LOGS=$(docker logs sauron-primary 2>&1 | grep "Endpoint validated successfully" | wc -l)

if [ "$VALIDATION_LOGS" -ge 3 ]; then
    print_pass "Endpoints validated successfully (found $VALIDATION_LOGS validations)"
    echo ""
    docker logs sauron-primary 2>&1 | grep "Endpoint validated successfully"
    echo ""
else
    print_fail "Endpoints not validated (found $VALIDATION_LOGS validations, expected >= 3)"
fi

# Test 6: Check Prometheus metrics
print_header "Test 6: Prometheus Metrics Verification"

run_test
print_test "Checking external endpoint tracking metrics"

METRICS=$(curl -s http://localhost:3000/metrics | grep "sauron_external_endpoints_tracked")
if echo "$METRICS" | grep -q "sauron_external_endpoints_tracked"; then
    print_pass "External endpoint tracking metrics found"
    echo ""
    echo "$METRICS"
    echo ""
else
    print_fail "External endpoint tracking metrics not found"
fi

run_test
print_test "Checking external endpoint validation metrics"

TRACKED=$(curl -s http://localhost:3000/metrics | grep 'sauron_external_endpoints_tracked.*secondary-ring' | grep -oE '[0-9]+$' | awk '{sum+=$1} END {print sum}')
VALIDATED=$(curl -s http://localhost:3000/metrics | grep 'sauron_external_endpoints_validated.*secondary-ring' | grep -oE '[0-9]+$' | awk '{sum+=$1} END {print sum}')
WORKING=$(curl -s http://localhost:3000/metrics | grep 'sauron_external_endpoints_working.*secondary-ring' | grep -oE '[0-9]+$' | awk '{sum+=$1} END {print sum}')

print_info "Tracked endpoints: $TRACKED"
print_info "Validated endpoints: $VALIDATED"
print_info "Working endpoints: $WORKING"

if [ "$TRACKED" -ge 3 ] && [ "$VALIDATED" -ge 3 ] && [ "$WORKING" -ge 3 ]; then
    print_pass "All endpoint types (API, RPC, gRPC) are tracked, validated, and working"
else
    print_fail "Not all endpoints are validated and working (Tracked: $TRACKED, Validated: $VALIDATED, Working: $WORKING)"
fi

run_test
print_test "Checking validation attempt counters"

VALIDATION_ATTEMPTS=$(curl -s http://localhost:3000/metrics | grep 'sauron_external_endpoint_validation_attempts_total.*result="success"' | grep -oE '[0-9]+$' | awk '{sum+=$1} END {print sum}')

print_info "Total successful validation attempts: $VALIDATION_ATTEMPTS"

if [ "$VALIDATION_ATTEMPTS" -ge 3 ]; then
    print_pass "Validation attempts recorded ($VALIDATION_ATTEMPTS successful)"
else
    print_fail "Insufficient validation attempts (found $VALIDATION_ATTEMPTS, expected >= 3)"
fi

run_test
print_test "Checking error counts"

ERROR_COUNT=$(curl -s http://localhost:3000/metrics | grep 'sauron_external_endpoint_error_count' | grep -oE '[0-9]+$' | awk '{sum+=$1} END {print sum}')

print_info "Total errors across all endpoints: $ERROR_COUNT"

if [ "$ERROR_COUNT" -eq 0 ]; then
    print_pass "No errors detected on external endpoints"
else
    print_fail "Errors detected on external endpoints (count: $ERROR_COUNT)"
fi

# Test 7: Verify endpoint types
print_header "Test 7: Endpoint Type Coverage"

for TYPE in api rpc grpc; do
    run_test
    print_test "Checking $TYPE endpoint validation"

    COUNT=$(curl -s http://localhost:3000/metrics | grep "sauron_external_endpoints_validated.*type=\"$TYPE\"" | grep -oE '[0-9]+$')

    if [ "$COUNT" -ge 1 ]; then
        print_pass "$TYPE endpoint validated (count: $COUNT)"
    else
        print_fail "$TYPE endpoint not validated"
    fi
done

# Test 8: Health check endpoints
print_header "Test 8: Health Check Endpoints"

run_test
print_test "Checking primary health endpoint"

PRIMARY_HEALTH=$(curl -s http://localhost:3000/health)
if [ "$PRIMARY_HEALTH" = "OK" ]; then
    print_pass "Primary health endpoint responding"
else
    print_fail "Primary health endpoint not responding correctly: $PRIMARY_HEALTH"
fi

run_test
print_test "Checking secondary health endpoint"

SECONDARY_HEALTH=$(curl -s http://localhost:4000/health)
if [ "$SECONDARY_HEALTH" = "OK" ]; then
    print_pass "Secondary health endpoint responding"
else
    print_fail "Secondary health endpoint not responding correctly: $SECONDARY_HEALTH"
fi

# Test 9: Status API returns external ring info
print_header "Test 9: Status API External Ring Information"

run_test
print_test "Checking if secondary status API advertises endpoints"

STATUS=$(curl -s http://localhost:4000/status)
if echo "$STATUS" | grep -q "endpoints"; then
    print_pass "Secondary status API contains endpoint information"
    echo ""
    echo "$STATUS" | head -30
    echo ""
else
    print_fail "Secondary status API missing endpoint information"
fi

# Test 10: Test tools are available in containers
print_header "Test 10: Container Tools Availability"

for TOOL in curl jq grpcurl wget; do
    run_test
    print_test "Checking if $TOOL is available in primary container"

    if docker exec sauron-primary which $TOOL >/dev/null 2>&1; then
        print_pass "$TOOL is available in primary container"
    else
        print_fail "$TOOL is NOT available in primary container"
    fi
done

# Test 11: Network connectivity between containers
print_header "Test 11: Container Network Connectivity"

run_test
print_test "Testing primary -> secondary connectivity"

if docker exec sauron-primary curl -s http://secondary:4000/health >/dev/null 2>&1; then
    print_pass "Primary can reach secondary via Docker network"
else
    print_fail "Primary cannot reach secondary via Docker network"
fi

run_test
print_test "Testing secondary -> primary connectivity"

if docker exec sauron-secondary curl -s http://primary:3000/health >/dev/null 2>&1; then
    print_pass "Secondary can reach primary via Docker network"
else
    print_fail "Secondary cannot reach primary via Docker network"
fi

# Test 12: Validation latency metrics
print_header "Test 12: Validation Latency Metrics"

run_test
print_test "Checking validation latency histogram"

LATENCY_METRICS=$(curl -s http://localhost:3000/metrics | grep 'sauron_external_endpoint_validation_latency_seconds_count' | grep 'secondary-ring')

if [ -n "$LATENCY_METRICS" ]; then
    print_pass "Validation latency metrics are being recorded"
    echo ""
    echo "$LATENCY_METRICS"
    echo ""
else
    print_fail "Validation latency metrics not found"
fi

# Final Summary
print_header "Test Summary"

echo -e "Total Tests Run:    ${BLUE}$TESTS_RUN${NC}"
echo -e "Tests Passed:       ${GREEN}$TESTS_PASSED${NC}"
echo -e "Tests Failed:       ${RED}$TESTS_FAILED${NC}"
echo ""

if [ $TESTS_FAILED -eq 0 ]; then
    echo -e "${GREEN}========================================${NC}"
    echo -e "${GREEN}ALL TESTS PASSED!${NC}"
    echo -e "${GREEN}========================================${NC}"
    echo ""
    echo "External endpoint validation is working correctly!"
    echo ""
    echo "What was tested:"
    echo "  ✓ Docker Compose environment setup"
    echo "  ✓ Container health and startup"
    echo "  ✓ External endpoint discovery from status API"
    echo "  ✓ Endpoint validation (connectivity checks)"
    echo "  ✓ Prometheus metrics tracking"
    echo "  ✓ All endpoint types (API, RPC, gRPC)"
    echo "  ✓ Error tracking and monitoring"
    echo "  ✓ Network connectivity between containers"
    echo "  ✓ Container tools availability"
    echo ""
    exit 0
else
    echo -e "${RED}========================================${NC}"
    echo -e "${RED}SOME TESTS FAILED!${NC}"
    echo -e "${RED}========================================${NC}"
    echo ""
    echo "Check the output above for details."
    echo ""
    exit 1
fi
