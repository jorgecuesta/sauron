# Sauron

> Height-aware routing proxy for blockchain nodes: Cosmos (Pocket Network), EVM, Solana and custom chains

**Sauron** sits in front of the nodes of one or more blockchain networks. For every network it checks each node's block height and health, and routes each client request (REST, CometBFT RPC, JSON-RPC, WebSocket, gRPC) to a healthy node at the highest height, spreading load round-robin among them. Several Sauron deployments can discover each other's endpoints and use them as a fallback when their own nodes fall behind.

> **Note:** Project name inspired by Tolkien's works. This is an open-source, non-commercial project with no affiliation to the Tolkien Estate.

---

## The Problem

Off-chain actors (RelayMiners, Gateways, Indexers, Wallets) need a node that is up and synced. Any node can fall behind or stop answering: sync delays, network partitions, resource limits, maintenance.

**Without a router in front:**
- A node that falls behind serves stale data, or rejects requests for heights it does not have yet.
- A node that goes down fails every request sent to it until someone repoints the client.

**With Sauron:**
- Requests go to a healthy node at the highest height, and fail over as soon as a health check or the circuit breaker excludes a node.
- One process serves many networks, each with its own listeners, checks and routing.
- Nodes that come back are put back in rotation automatically.

---

## Prerequisites

- Go 1.27 or newer (see `go.mod`), or Docker
- The nodes you want to route to
- Free ports for the status API and for each network endpoint you configure

---

## Quick Start

### 1. Install

```bash
git clone https://github.com/pokt-network/sauron
cd sauron
make build
```

### 2. Configure

A network declares its chain `type` and the endpoints Sauron listens on; each internal node gives one URL per protocol. Create `config.yaml`:

```yaml
listen: ":3000"            # status API, /health, /ready, /metrics

timeouts:
  health_check: 5s         # timeout of each check
  proxy: 60s               # timeout of each proxied request

networks:
  - name: pocket
    type: cosmos
    height_check:
      protocol: rpc        # read the height through the RPC endpoint
      interval: 30s        # how often this network's nodes are checked
    endpoints:
      - protocol: rest
        listen: ":8080"
      - protocol: rpc      # also serves WebSocket upgrades (/websocket)
        listen: ":8081"
      - protocol: grpc
        listen: ":8082"

internals:
  - name: node-1
    network: pocket
    grpc_insecure: true
    endpoints:
      rest: "http://node1:1317"
      rpc: "http://node1:26657"
      grpc: "node1:9090"
  - name: node-2
    network: pocket
    grpc_insecure: true
    endpoints:
      rest: "http://node2:1317"
      rpc: "http://node2:26657"
      grpc: "node2:9090"
```

[`config.default.yaml`](config.default.yaml) is a fuller, commented example, including EVM and sync settings.

### 3. Run

```bash
./bin/sauron -config config.yaml
```

### 4. Use It

Point your clients at the network's endpoints:

```yaml
# RelayMiner, Gateway, Indexer, ...
node_api_url:  "http://localhost:8080"   # REST
node_rpc_url:  "http://localhost:8081"   # CometBFT RPC and WebSocket
node_grpc_url: "localhost:8082"          # gRPC
```

---

## Chains

Each network has a `type`. The type's adapter knows how to read the height and check the health of that chain's nodes, so most networks need no check configuration at all.

| `type` | Height check | Health checks | Options |
|---|---|---|---|
| `cosmos` | RPC `/abci_info` (`last_block_height`, the app-committed height); REST `/cosmos/base/tendermint/v1beta1/blocks/latest` or gRPC `GetLatestBlock` when `height_check.protocol` is `rest` or `grpc` | REST `blocks/latest`, RPC `/status`, RPC WebSocket subscription, gRPC `GetLatestBlock` | `height_check.protocol`: `rpc` (default), `rest`, `grpc` |
| `evm` | JSON-RPC `eth_blockNumber`, or `eth_getBlockByNumber` with the `safe`/`finalized` tag | JSON-RPC `net_version`, WebSocket | `mode`: `latest` (default), `safe`, `finalized` |
| `solana` | JSON-RPC `getSlot` (default) or `getBlockHeight` | JSON-RPC `getHealth`, WebSocket | `height_check.protocol`: `getSlot` or `getBlockHeight`; `mode`: commitment, `finalized` (default), `confirmed`, `processed` |
| `custom` | Whatever `height_check` describes: `method`, `url_path`, `headers`, `body`, `response_path` (JSONPath), `response_format` (`integer`, `hex`, `string`) | HTTP or JSON-RPC reachability, WebSocket | `height_check.*` |

Endpoint protocols are `rest`, `rpc`, `jsonrpc`, `http` (served by the HTTP proxy, which also proxies WebSocket upgrades to WebSocket-capable nodes) and `grpc` (a transparent gRPC proxy that passes headers, trailers and status codes through).

An EVM network, for example:

```yaml
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: ethereum
    type: evm
    mode: finalized
    height_check:
      interval: 12s
    endpoints:
      - protocol: jsonrpc
        listen: ":9080"
internals:
  - name: geth-1
    network: ethereum
    endpoints:
      jsonrpc: "http://geth-1:8545"
```

---

## How It Works

```
┌──────────────┐      ┌────────────────────────────┐      ┌──────────────────┐
│   Clients    │─────▶│ Sauron                     │─────▶│ chosen node      │
│ (RelayMiner, │      │  per network:              │      │ (healthy, at the │
│  Gateway,    │      │   • height + health checks │      │  max height)     │
│  Indexer)    │      │   • circuit breaker        │      │                  │
└──────────────┘      │   • selector               │      └──────────────────┘
                      └────────────────────────────┘
```

**Checks.** Every `height_check.interval` (default 30s) Sauron reads each node's height and checks each of its protocols. A node is routable for a protocol only while that protocol's check passes. The gRPC check makes a real query, so a node whose transport is up but whose queries fail stays out.

**Selection.** For each request, the selector:
1. takes the network's nodes that are healthy for the requested protocol (and, if configured, archival and in sync with the oracles);
2. adds external endpoints when the internals are more than `external_failover_threshold` blocks behind (default 2) or none is left;
3. keeps the nodes at the highest height;
4. picks one round-robin.

WebSocket upgrades only go to nodes whose WebSocket endpoint works.

**Circuit breaker (optional, per network).** Consecutive failures from a node (HTTP codes in `http_codes`, connection errors, gRPC node-failure codes) take it out of rotation after `threshold` errors; the health check puts it back once it answers. HTTP requests are retried up to `retry_attempts` times. Client cancels and answers about the request (e.g. a gRPC `NotFound`, an HTTP 400) never count.

---

## Configuration

### Per network

| Key | Purpose |
|---|---|
| `type` | `cosmos`, `evm`, `solana` or `custom` |
| `endpoints[]` | `protocol`, `listen`, optional `advertise` URL (what other Sauron rings see), gRPC message size limits |
| `height_check` | `protocol` the height is read through, `interval`; for `custom`, the request and the JSONPath to the height |
| `mode` | EVM block tag or Solana commitment |
| `circuit_breaker` | `threshold`, `http_codes`, `grpc_codes` (default `unavailable, internal, unknown, data_loss`), `retry_attempts`, `retry_backoff` |
| `archival` | `min_height`: only route to nodes that serve that historical block; `check_interval` |
| `sync` | `oracles` (URLs, optionally with their own check), `max_drift`: exclude nodes more than that many blocks behind the oracles; `check_interval` |

### Global

| Key | Purpose |
|---|---|
| `listen` | Status API address |
| `timeouts` | `health_check`, `proxy`, `websocket_idle` (close a WebSocket after this long without bytes from the node; default 90s) |
| `external_failover_threshold` | Blocks behind before external endpoints are used (default 2) |
| `externals[]` | Other Sauron deployments: `name`, `token`, `rings` (their status API URLs) |
| `auth`, `users[]` | Bearer tokens for the **status API** (what external rings query); `permissions: all` or per network and protocol. The proxy ports are not authenticated |
| `rate_limit` | Requests per second and burst on the status API |
| `redis` | Optional cache of node heights |
| `grpc_keepalive` | Keepalive for Sauron's gRPC connections to nodes |

### Hot reload

Edit the file (it is watched) or send `SIGHUP`. Nodes, externals, users, check intervals and the health-check timeout, circuit breakers, archival `min_height` and advertise URLs apply at once; removed nodes and externals are forgotten (their state, metrics and pooled connections). Startup-only fields (`listen`, `auth`, `rate_limit`, `redis`, `timeouts.proxy`, `grpc_keepalive`, the set of networks and their listeners, `archival` on/off, `sync`, `mode`, `height_check` other than `interval`) keep their running values until a restart; the rest of the file is still applied, and `sauron_config_reload_ok` shows 0 until then. A file that fails validation is rejected and the running config stays.

---

## Status API

| Path | |
|---|---|
| `GET /health` | Liveness |
| `GET /ready` | 200 once at least one internal node is configured (it does not check that a node is routable) |
| `GET /{network}/status` | The network's height and advertised endpoints (what external rings read); behind `auth` and `rate_limit` when enabled |
| `GET /metrics` | Prometheus metrics |

Main metrics: `sauron_node_height`, `sauron_node_available`, `sauron_node_websocket_available`, `sauron_routing_selections_total`, `sauron_routing_failures_total`, `sauron_node_requests_total`, `sauron_proxy_request_duration_seconds`, `sauron_proxy_errors_total` (by `error_type`), `sauron_config_reload_ok`, and the `sauron_external_*` family for external rings.

---

## Testing

```bash
make test                   # unit and pipeline tests, with the race detector
make ci                     # everything CI runs: test, lint, format check
make docker-test-protocols  # L3: every protocol and failover against fake nodes (no network; runs in CI)
make docker-test-advanced   # L3: external rings, reload, Redis, auth, rate limiting
make mutation               # mutation testing (gremlins); also runs nightly in CI
```

The L3 suites' host ports are configurable (`SAURON_*_PORT`); see [.docker/TESTING.md](.docker/TESTING.md).

---

## Development

```bash
make build    # Build bin/sauron
make fmt      # Format code
make lint     # golangci-lint (pinned)
make clean    # Remove build artifacts
```

Architecture details: [HOW_THIS_WORKS.md](HOW_THIS_WORKS.md).

---

## License

MIT License, see [LICENSE](LICENSE).

## Contributing

Contributions welcome: open an issue or a PR.
