# Docker Testing Environment

This directory contains Docker Compose setup for testing Sauron with external endpoint validation.

## Files

- `Dockerfile` - Multi-stage build for Sauron container
- `docker-compose.yaml` - Primary + Secondary ring setup
- `config-primary-docker.yaml` - Primary configuration (monitors secondary as external)
- `config-secondary-docker.yaml` - Secondary configuration (leaf node)
- `test-external-validation.sh` - Basic endpoint validation tests (23 tests)
- `test-advanced-features.sh` - Advanced feature tests (15 tests)

IMPORTANT: you need to provide valid endpoints on the `config-[primary|secondary]-docker.yaml` to test

## Quick Start

From project root:

```bash
# Start environment
make docker-up

# Run tests
make docker-test                # Basic validation
make docker-test-advanced       # Advanced features

# Stop environment
make docker-down
```

## Architecture

```
┌─────────────┐           ┌──────────────┐
│   Primary   │ monitors  │  Secondary   │
│   (3000)    │──────────▶│   (4000)     │
│             │ /status   │              │
│ - Proxy API │           │ - API  :7080 │
│ - Proxy RPC │           │ - RPC  :7081 │
│ - Proxy gRPC│           │ - gRPC :7082 │
└─────────────┘           └──────────────┘
```

Primary discovers and validates external endpoints from Secondary's status API,
then includes them in routing decisions alongside internal nodes.

## Running alongside other services

Host ports default to 3000/4000 (status), 8080-8082 and 7080-7082 (proxies) and 6379 (Redis). If something else holds them, override any of them:

```bash
SAURON_PRIMARY_STATUS_PORT=23000 SAURON_SECONDARY_STATUS_PORT=24000 SAURON_REDIS_PORT=26379 \
SAURON_PRIMARY_API_PORT=28080 SAURON_PRIMARY_RPC_PORT=28081 SAURON_PRIMARY_GRPC_PORT=28082 \
SAURON_SECONDARY_API_PORT=27080 SAURON_SECONDARY_RPC_PORT=27081 SAURON_SECONDARY_GRPC_PORT=27082 \
make docker-test
```
