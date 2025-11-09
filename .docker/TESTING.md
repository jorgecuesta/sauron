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
