# Docker Testing Environment

Two L3 environments live here:

- **Fake nodes** (`docker-compose.fake.yaml`): Sauron in front of two deterministic fake Cosmos nodes (`internal/fakenode`, built by `Dockerfile.fakenode`). No network access needed; it runs in CI on every PR.
- **Primary + secondary rings** (`docker-compose.yaml`): two Sauron deployments, the primary using the secondary as an external ring, in front of real nodes.

## Files

- `Dockerfile` - Multi-stage build for the Sauron container
- `Dockerfile.fakenode`, `docker-compose.fake.yaml`, `config-fake.yaml` - Fake-node environment
- `test-protocols.sh` - Every client path (REST, RPC GET/POST, WebSocket, gRPC with metadata), failover, recovery and all-nodes-down, against fake nodes (9 checks)
- `docker-compose.yaml` - Primary + secondary ring setup
- `config-primary-docker.yaml` - Primary configuration (monitors the secondary as an external)
- `config-secondary-docker.yaml` - Secondary configuration
- `test-external-validation.sh` - External endpoint validation (19 tests)
- `test-advanced-features.sh` - External failover and recovery, hot reload, Redis, auth, rate limiting, status endpoints, endpoint state transitions (29 tests)

The ring environment needs reachable nodes in `config-[primary|secondary]-docker.yaml`.

## Quick Start

From project root:

```bash
make docker-test-protocols      # fake nodes; no network needed

make docker-test                # ring environment: external validation
make docker-test-advanced       # ring environment: advanced features

make docker-up                  # or keep the ring environment up to test by hand
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

The ring environment's host ports default to 3000/4000 (status), 8080-8082 and 7080-7082 (proxies) and 6379 (Redis). If something else holds them (check with `ss -ltnp`), override any of them:

```bash
SAURON_PRIMARY_STATUS_PORT=23000 SAURON_SECONDARY_STATUS_PORT=24000 SAURON_REDIS_PORT=26379 \
SAURON_PRIMARY_API_PORT=28080 SAURON_PRIMARY_RPC_PORT=28081 SAURON_PRIMARY_GRPC_PORT=28082 \
SAURON_SECONDARY_API_PORT=27080 SAURON_SECONDARY_RPC_PORT=27081 SAURON_SECONDARY_GRPC_PORT=27082 \
make docker-test
```

The fake-node environment uses `SAURON_FAKE_*_PORT` (defaults 33000 and 38080-38082, admin 38100-38101).
