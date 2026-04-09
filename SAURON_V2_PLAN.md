# Sauron V2 — Multi-Chain Blockchain Node Router

## Vision

Sauron evolves from a Pocket Network (Cosmos SDK) routing proxy into a **universal blockchain node load balancer**. It monitors node block heights across any chain type (Cosmos, EVM, Solana, custom) and routes client requests to the healthiest, most up-to-date node.

## Architecture Principles

### The Custom Engine IS the Runtime

There is ONE execution engine. Native adapters (cosmos, evm, solana) are **factories** that produce configuration for that engine. They do NOT have their own runtime code.

```
┌─────────────┐     ┌──────────────┐
│ cosmos       │────→│              │
│ factory      │     │              │
├─────────────┤     │   Engine     │     ┌──────────┐
│ evm          │────→│  (executes   │────→│ HeightStore
│ factory      │     │  CheckConfig)│     │ HealthStore
├─────────────┤     │              │     └──────────┘
│ solana       │────→│              │
│ factory      │     │              │
├─────────────┤     │              │
│ custom       │────→│              │
│ (passthrough)│     └──────────────┘
└─────────────┘
```

If EVM and Solana both do "POST JSON-RPC, parse response" — that's ONE code path in the engine. The factory just provides different request bodies and parse rules.

### Height Check vs Health Check

**Height check**: 1 per network, 1 protocol. Determines the node's current block height. This is the source of truth for routing decisions.

**Health check**: 1 per endpoint protocol. Determines if that specific protocol is responding. A node can have good height but broken gRPC — Sauron routes HTTP traffic to it but not gRPC.

```
Node "eth-geth-1":
  height_check (jsonrpc) → height: 20,000,000  ✓
  health_check (jsonrpc) → responding           ✓
  health_check (websocket) → not responding     ✗

Result: routable via jsonrpc, NOT via websocket
```

### Selection Pipeline

```
For each request:
  1. Get all nodes for (network, protocol)
  2. Filter: node has height > 0 (height check passed)
  3. Filter: node's endpoint for this protocol is healthy
  4. Filter: if archival configured → node.HasBlock(min_height)
  5. Filter: if sync configured → |oracle_height - node_height| <= max_drift
  6. Select: max height among remaining → sorted by name → round-robin
```

Steps 4-5 are optional filters. Steps 1-3 and 6 are the existing v1 logic (proven in production).

## Config Format V2

```yaml
listen: ":3000"
auth: true

timeouts:
  health_check: 5s
  proxy: 300s

grpc_keepalive:
  time: 600s
  timeout: 20s
  permit_without_stream: true

rate_limit:
  enabled: true
  requests_per_second: 100
  burst: 200
  trust_proxy: true

redis:
  enabled: false

# ─── Networks ────────────────────────────────────────────────────────────────

networks:

  # Cosmos — native adapter
  - name: pocket
    type: cosmos
    height_check:
      protocol: rpc                 # Which protocol to use for height tracking
      interval: 30s
    endpoints:
      - protocol: rest
        listen: ":8080"
        advertise: "https://sauron-api.infra.pocket.network"
      - protocol: rpc
        listen: ":8081"
        advertise: "https://sauron-rpc.infra.pocket.network"
      - protocol: grpc
        listen: ":8082"
        advertise: "https://sauron-grpc.infra.pocket.network"
        grpc_max_recv_msg_size: 104857600
        grpc_max_send_msg_size: 104857600

  # EVM — native adapter
  - name: ethereum
    type: evm
    mode: finalized                 # latest | safe | finalized
    height_check:
      interval: 12s
    endpoints:
      - protocol: jsonrpc
        listen: ":9080"
        advertise: "https://sauron-eth.infra.pocket.network"
    archival:
      min_height: 1
      check_interval: 5m
    sync:
      max_drift: 5
      check_interval: 30s
      oracles:
        - "https://rpc.ankr.com/eth"

  # Custom — operator defines everything
  - name: mina
    type: custom
    height_check:
      interval: 30s
      method: POST
      url_path: /graphql
      headers:
        Content-Type: application/json
      body: '{"query":"{ bestChain(maxLength:1) { protocolState { consensusState { blockHeight } } } }"}'
      response_path: ".data.bestChain[0].protocolState.consensusState.blockHeight"
      response_format: integer
    endpoints:
      - protocol: http
        listen: ":9100"
        advertise: "https://sauron-mina.infra.pocket.network"
    sync:
      max_drift: 5
      check_interval: 60s
      oracles:
        - "https://proxy.minaexplorer.com"    # Same type → uses height_check config
        - url: "https://api.minaexplorer.com" # Different API → override
          method: GET
          url_path: /blocks?limit=1
          response_path: ".blocks[0].blockHeight"
          response_format: integer

# ─── Internal Nodes ──────────────────────────────────────────────────────────

internals:

  # Cosmos nodes — multiple protocol endpoints per node
  - name: seed-one
    network: pocket
    endpoints:
      rest: "http://seed-one.mainnet.svc.cluster.local:1317"
      rpc: "http://seed-one.mainnet.svc.cluster.local:26657"
      grpc: "seed-one.mainnet.svc.cluster.local:9090"
      grpc_insecure: true

  # EVM nodes — single endpoint
  - name: eth-geth-1
    network: ethereum
    endpoints:
      jsonrpc: "http://geth-1.internal:8545"
      # websocket: "ws://geth-1.internal:8546"  # Only if different port

  # Custom nodes
  - name: mina-node-1
    network: mina
    endpoints:
      http: "http://mina-1.internal:3085"

# ─── Externals ───────────────────────────────────────────────────────────────

externals:
  - name: us-west-sauron
    token: "shared-secret"
    rings:
      - "https://sauron-us-west.example.com:3000"

# ─── Users ───────────────────────────────────────────────────────────────────

users:
  - name: indexer-service
    token: "idx-secret-token"
    permissions:
      pocket:
        rest: true
        rpc: true
        grpc: false
      ethereum:
        jsonrpc: true
  - name: relayminer-prod
    token: "rm-secret-token"
    permissions: all
```

## Package Structure

```
sauron/
├── adapter/
│   ├── adapter.go              # ChainAdapter interface
│   ├── check.go                # CheckConfig, HealthCheckConfig structs
│   ├── engine.go               # ONE runtime: executes CheckConfig → int64
│   ├── engine_test.go          # Exhaustive tests with testdata/custom/ fixtures
│   ├── health.go               # Health check runner (per-endpoint alive/dead)
│   ├── health_test.go
│   ├── registry.go             # Adapter registry: type name → factory
│   ├── registry_test.go
│   ├── cosmos/
│   │   ├── factory.go          # Produces CheckConfig for cosmos (rest, rpc, grpc)
│   │   └── factory_test.go
│   ├── evm/
│   │   ├── factory.go          # Produces CheckConfig for evm (jsonrpc, ws)
│   │   └── factory_test.go
│   ├── solana/
│   │   ├── factory.go          # Produces CheckConfig for solana
│   │   └── factory_test.go
│   └── testdata/
│       ├── custom/             # Engine test fixtures (THE most comprehensive)
│       │   ├── height_integer.json
│       │   ├── height_hex.json
│       │   ├── height_string_number.json
│       │   ├── height_nested_deep.json
│       │   ├── height_array_index.json
│       │   ├── height_null.json
│       │   ├── height_empty_object.json
│       │   ├── height_not_json.txt
│       │   ├── height_wrong_type_bool.json
│       │   ├── height_wrong_type_float.json
│       │   ├── height_negative.json
│       │   ├── height_overflow.json
│       │   ├── height_zero.json
│       │   ├── height_graphql_ok.json
│       │   ├── height_graphql_error.json
│       │   ├── height_jsonrpc_error.json
│       │   ├── health_ok_200.txt
│       │   ├── health_server_error_500.txt
│       │   ├── archival_exists.json
│       │   ├── archival_null.json
│       │   └── archival_not_found.json
│       ├── cosmos/
│       │   ├── rpc_status_ok.json
│       │   ├── rpc_status_syncing.json
│       │   ├── rpc_status_malformed.json
│       │   ├── rest_blocks_latest_ok.json
│       │   ├── grpc_abci_ok.json
│       │   ├── archival_block_ok.json
│       │   └── archival_block_not_found.json
│       ├── evm/
│       │   ├── eth_blockNumber_latest.json
│       │   ├── eth_blockNumber_finalized.json
│       │   ├── eth_blockNumber_safe.json
│       │   ├── eth_blockNumber_hex_invalid.json
│       │   ├── eth_blockNumber_error.json
│       │   ├── eth_chainId_ok.json
│       │   ├── eth_getBlockByNumber_ok.json
│       │   └── eth_getBlockByNumber_null.json
│       └── solana/
│           ├── getSlot_ok.json
│           ├── getSlot_error.json
│           ├── getBlockHeight_ok.json
│           ├── getBlock_ok.json
│           └── getBlock_null.json
├── checker/
│   ├── scheduler.go            # Cron-based scheduler (uses adapter engine)
│   ├── scheduler_test.go
│   └── common.go               # Shared helpers (metrics recording, etc.)
├── selector/
│   ├── selector.go             # Core selection + archival/sync filters
│   ├── selector_test.go
│   ├── filter.go               # Archival and sync filter implementations
│   └── filter_test.go
├── proxy/
│   ├── http_proxy.go           # HTTP reverse proxy (rest, jsonrpc, graphql)
│   ├── http_proxy_test.go
│   ├── grpc_proxy.go           # gRPC transparent proxy
│   ├── grpc_proxy_test.go
│   ├── websocket.go            # WebSocket proxy (extracted, shared)
│   └── websocket_test.go
├── storage/
│   ├── heights.go              # HeightStore (node → height, per network)
│   ├── heights_test.go
│   ├── health.go               # HealthStore (node:protocol → alive/dead)
│   ├── health_test.go
│   ├── external_endpoints.go
│   ├── external_endpoints_test.go
│   └── cache.go
├── config/
│   ├── config.go               # V2 config structs
│   ├── config_test.go
│   ├── loader.go               # Viper loader + hot reload
│   ├── loader_test.go
│   ├── validator.go            # V2 config validation
│   └── validator_test.go
├── status/                     # Status API (unchanged pattern, extended for multi-network)
├── metrics/                    # Prometheus metrics
├── server/                     # Orchestrator + lifecycle
├── internal/
│   ├── urlutil/                # URL normalization
│   └── jsonpath/               # JSON path extractor (used by engine)
│       ├── extract.go          # Extract value from JSON by dot-path
│       └── extract_test.go
└── main.go
```

## Implementation Phases

### Phase 1: Foundation (adapter engine + config v2)

**Goal**: The custom engine works, config v2 parses, but only cosmos adapter runs (existing behavior preserved).

1. `internal/jsonpath/` — JSON path extractor with exhaustive tests
2. `adapter/` — CheckConfig structs, Engine (execute check → int64), HealthCheck runner
3. `adapter/cosmos/` — Factory that produces CheckConfig from cosmos network config
4. `config/` — V2 config structs + parser + validator
5. `storage/health.go` — HealthStore (node:protocol → alive/dead)
6. `checker/` — Refactor scheduler to use adapter engine instead of hardcoded checkers
7. `selector/` — Accept HealthStore, filter by endpoint health
8. Wire it all together in `server/` — existing behavior, new internals

**Validation**: Deploy to beta, run existing test scripts, verify identical behavior.

### Phase 2: EVM adapter + archival/sync filters

**Goal**: EVM networks work. Archival and sync filters work for all types.

1. `adapter/evm/` — Factory for EVM (latest/safe/finalized modes)
2. `selector/filter.go` — Archival filter + sync/oracle filter
3. `checker/` — Oracle height check scheduling
4. Tests with EVM fixtures

**Validation**: Stand up a test EVM network in config, verify height tracking + routing.

### Phase 3: Solana adapter + custom adapter + multi-protocol proxy

**Goal**: Full v2 feature set.

1. `adapter/solana/` — Factory for Solana (slot/block_height, commitment levels)
2. Custom adapter passthrough (height_check config → engine directly)
3. Proxy refactor: protocol-driven listener creation (not hardcoded api/rpc/grpc)
4. WebSocket extraction into shared module
5. User permissions per network:protocol

### Phase 4: Migration + release

1. Migration guide document (v1 config → v2 config)
2. Status API extended for multi-network multi-protocol
3. External ring discovery extended for multi-chain
4. Production deployment + testing

## V2 Engineering Rules

These are NON-NEGOTIABLE for every line of code in v2:

1. **Interfaces everywhere.** If two chain types do the same thing differently, that difference lives behind an interface — not in `if/else` or `switch` on chain type.

2. **One path, one implementation.** If height checking, archival checking, and oracle checking all execute an HTTP request and extract a JSON value, that is ONE function. The config varies, the code does not.

3. **100% test coverage on new code.** Every adapter, every filter, every parser. Table-driven tests with fixtures. Happy paths AND failure paths. If it is not tested, it does not exist.

4. **No code duplication. Zero.** If the same logic appears in two places, extract it immediately. EVM and Solana both doing JSON-RPC POST + parse → one shared path in the engine.

5. **Modular packages.** Each adapter is its own package. Adding a new chain type = adding one factory package that satisfies the interface. Zero changes to core code.

6. **Height check: 1 protocol per network.** Other endpoints get health checks only (alive/dead). Height and health are independent concerns.

7. **Custom is the engine, native is sugar.** Native adapters are factories that produce CheckConfig. They share the engine runtime. No separate code paths.

8. **All quality gates, every commit.** Build, test, race, vet, lint, format. No exceptions. No "pre-existing" excuses.

## Token Efficiency Strategy

To maximize quality while minimizing token consumption:

- **Opus**: Architecture decisions, plan review, integration audits only
- **Sonnet**: All implementation work (write code, write tests, refactor)
- **Haiku**: Mechanical tasks (renames, fixture file creation, format fixes)
- **Parallel agents**: Only for truly independent packages with zero file overlap
- **Self-contained phases**: Each phase produces a working state. No "half-done" phases that need context carryover.
- **Fixture-first**: Write test fixtures before implementation. Fixtures are cheap (JSON files), tests are cheap (table-driven), and they define the contract before code exists.

## Files to Reference

When starting a fresh session for implementation:

- This file: `SAURON_V2_PLAN.md` — the complete plan
- `CLAUDE.md` — coding standards, quality gates, agent rules
- `config/config.go` — current v1 config structs (to understand what exists)
- `checker/` — current checker implementations (to understand what to replace)
- `selector/selector.go` — current selection logic (to understand what to extend)
- `proxy/http_proxy.go` — current HTTP proxy (to understand what stays)
- `proxy/grpc_proxy.go` — current gRPC proxy (to understand what stays)
