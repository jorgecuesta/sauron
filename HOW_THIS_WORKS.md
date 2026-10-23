# How Sauron Works

Sauron is a multi-chain routing proxy. For each configured network it checks the height and per-protocol health of that network's nodes, and routes every client request to a healthy node at the highest height, round-robin among them. Configuration and the per-chain checks are summarised in the [README](README.md); this document covers the internals.

## Architecture

```
                     ┌──────────────────────── Sauron ────────────────────────┐
┌─────────┐ request  │  ┌─────────────┐   pick    ┌──────────┐                 │
│ Clients │─────────▶│  │ HTTP / gRPC │──────────▶│ Selector │                 │
└─────────┘          │  │   proxies   │◀──────────│          │                 │
                     │  └──────┬──────┘   node    └────┬─────┘                 │
                     │         │ breaker errors       │ reads                 │
                     │         ▼                      ▼                       │
                     │  ┌─────────────────────────────────────┐              │
                     │  │ HeightStore · HealthStore ·          │◀── checks ──┤◀── nodes
                     │  │ ArchivalStore · OracleStore ·        │   (scheduler,│
                     │  │ ExternalEndpointStore               │    adapters) │
                     │  └─────────────────────────────────────┘              │
                     └─────────────────────────────────────────────────────────┘
```

## Components

### Adapters (`adapter/`)

One adapter per network `type`: `cosmos`, `evm`, `solana`, `custom`. Given a network's config, an adapter returns the request that reads a node's height (method, path, body, JSONPath to the height, number format) and the health checks for each protocol the node exposes. The check engine (`adapter/engine.go`) runs those requests; nothing else in Sauron is chain-specific.

For Cosmos the height is the app-committed height from RPC `/abci_info`, not the blockstore height in `/status`: CometBFT saves a block before the app commits it, so `/status` can run a block ahead of what the node can answer.

### Scheduler (`checker/v2_scheduler.go`)

A one-second tick runs each network's checks when its interval has elapsed: node checks every `height_check.interval`, archival checks every `archival.check_interval`, oracle checks every `sync.check_interval` (all default 30s, read from the current config on every tick). A node's check is never started again while the previous one is still running.

For each internal node:
- **Height:** through the network's height-check protocol. A result updates the HeightStore and marks that protocol healthy or unhealthy.
- **Health per protocol:** HTTP checks per the adapter; a WebSocket subscription check for `rpc`/`websocket`; for gRPC, the transport must reach READY and then a real `GetLatestBlock` query must not fail with a node-failure status (`Unavailable`, `Internal`, `Unknown`, `DataLoss`) or time out. Any other answer, `Unimplemented` included, means the node serves.

External rings are queried every 10s, and failed external endpoints are re-checked every 10s.

### Selector (`selector/`)

`GetBestNode(network, protocol)`:
1. Internal nodes with a height, kept only if healthy for the requested protocol.
2. Archival filter (when `archival` is set) and sync filter (when `sync` is set: drop nodes more than `max_drift` behind the oracles).
3. External endpoints are added when no internal is left or the best external is more than `external_failover_threshold` blocks ahead.
4. Nodes at the maximum height, sorted by name, picked round-robin per network and protocol.

`GetBestWebSocketNode` does the same restricted to WebSocket-capable nodes (before the height comparison, so a WebSocket node a block behind beats none), with its own round-robin counter.

### Proxies (`proxy/`)

- **HTTP** (`rest`, `rpc`, `jsonrpc`, `http`): buffers the body (up to 10 MB; larger is 413) for retries, forwards to the selected node keeping the path's escaping, and on a connection error or a code in `circuit_breaker.http_codes` records a breaker error and retries on a newly selected node, up to `retry_attempts`. A client that cancels is neither retried nor counted against the node.
- **WebSocket** (an upgrade on an HTTP endpoint): routed only to WebSocket-capable nodes. A non-101 answer is forwarded and reported as `upgrade_rejected`; it counts against the node only for codes in `http_codes`. During the session, a node that sends nothing for `timeouts.websocket_idle` (default 90s) is disconnected (`websocket_idle_timeout`); the copy uses splice.
- **gRPC**: a transparent proxy over raw frames (any service, no proto types), speaking `application/grpc+proto`. Response headers, trailers and status codes pass through unchanged. Node-failure codes (or the network's `grpc_codes`) feed the breaker; other answers reset it. An unreachable node is `Unavailable`.

### Circuit breaker (`proxy/circuit_breaker.go`)

Per network, node and protocol: `threshold` consecutive errors mark the protocol unhealthy in the HealthStore, so the selector skips it; the next passing health check marks it healthy again. A success resets the count.

### Stores (`storage/`)

HeightStore (height, latency, WebSocket availability), HealthStore (per protocol), ArchivalStore, OracleStore, ExternalEndpointStore, and an optional Redis cache of heights. All are safe for concurrent use.

### Reload (`config/`, `server/`)

The config file is watched, and `SIGHUP` reloads it. A file that fails validation is rejected. Startup-only fields keep their running values (the list is in `config.keepStartupOnly`); everything else applies at once. After every applied reload, Sauron forgets removed nodes and externals (store entries, breaker counts, state metrics) and closes pooled gRPC connections to addresses nothing routes to; it does this again once the health-check timeout has passed, in case a check that was running wrote a removed node back.

## External Endpoint Integration

Several Sauron deployments can back each other up. Each one lists the others under `externals`; every 10s it reads `GET <ring>/{network}/status` from each ring (with the ring's token), stores the endpoints the ring advertises, and validates them directly (HTTP reachability, or a gRPC `GetLatestBlock`). A ring does not say whether its gRPC endpoint speaks TLS or plaintext: validation tries the transport that last worked (TLS the first time), then the other, and the proxy dials the endpoint with whichever answered.

An endpoint goes `ADVERTISED → VALIDATED → FAILED → RECOVERED`. A validated endpoint that fails validation, or reaches 3 proxy errors, is FAILED: kept out of routing and re-checked every 10s until it answers. When a ring stops answering, its endpoints are validated directly instead of trusting their last result.

| Internal height | External height | Threshold | Externals used? |
|---|---|---|---|
| 100 | 102 | 2 | No (102 is not more than 100 + 2) |
| 100 | 103 | 2 | Yes |
| none healthy | 100 | 2 | Yes |

## Authentication

`auth: true` puts bearer tokens on the **status API** (`/{network}/status`, what other rings read); `users[].permissions` limits a token to networks and protocols (`all`, or a map). `/health`, `/ready` and `/metrics` are open. **The proxy ports have no authentication**: expose them only to the clients that should use them.

## Monitoring

`GET /metrics` on the status port. The main series, with their labels:

| Metric | Labels | Meaning |
|---|---|---|
| `sauron_node_height` | network, node, type, source | Last height read (`source`: internal/external) |
| `sauron_node_available` | network, node, type | 1 when the last height check passed |
| `sauron_node_websocket_available` | network, node, type | 1 when the WebSocket check passed |
| `sauron_node_latency_seconds` | network, node, type | Check latency |
| `sauron_height_check_errors_total` | network, node, type, error_type | Failed height checks |
| `sauron_routing_selections_total` | network, type, node_source, reason | Selections (`reason`: height_winner, round_robin, only_available) |
| `sauron_routing_failures_total` | network, type, reason | No node to route to (`no_nodes`, `no_websocket_nodes`, `zero_height`) |
| `sauron_node_requests_total` | network, node, type, method | Requests per node (`method`: HTTP verb, `WEBSOCKET`, gRPC service) |
| `sauron_proxy_request_duration_seconds` | network, node, type, status | Proxied request duration |
| `sauron_proxy_errors_total` | network, node, type, status_code, error_type | Proxy errors, e.g. `retriable`, `http_error`, `client_cancel`, `body_too_large`, `upgrade_rejected`, `websocket_idle_timeout`, `client_disconnect`; for gRPC, `backend_status`, `client_error`, `proxy_error` |
| `sauron_config_reload_ok` | — | 0 while a reload is rejected or left startup-only changes pending |
| `sauron_external_endpoints_{tracked,validated,working}` | network, type, ring_name | External endpoints by state |
| `sauron_external_endpoint_recoveries_total` | network, type, ring_name | FAILED → working transitions |

Example PromQL:

```promql
# Nodes at their network's max height
sauron_node_height == on (network, type) group_left max by (network, type) (sauron_node_height)

# Request rate per node
sum by (network, node) (rate(sauron_node_requests_total[5m]))

# P95 proxied latency per network
histogram_quantile(0.95, sum by (network, le) (rate(sauron_proxy_request_duration_seconds_bucket[5m])))

# Error ratio by kind
sum by (network, error_type) (rate(sauron_proxy_errors_total[5m]))
```

## Production Deployment

Run one Sauron per region, each with its region's nodes as internals and the other regions as externals:

```yaml
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: rest
        listen: ":8080"
        advertise: "https://api.region1.example.com"
      - protocol: rpc
        listen: ":8081"
        advertise: "https://rpc.region1.example.com"
      - protocol: grpc
        listen: ":8082"
        advertise: "grpc.region1.example.com:443"
internals:
  - name: region1-node-1
    network: pocket
    endpoints:
      rest: "http://node1.region1.internal:1317"
      rpc: "http://node1.region1.internal:26657"
      grpc: "node1.region1.internal:9090"
    grpc_insecure: true
externals:
  - name: region2
    token: "shared-secret-region2"
    rings: ["https://sauron.region2.example.com:3000"]
auth: true
users:
  - name: region2
    token: "shared-secret-region1"
    permissions: all
```

### Docker and Kubernetes

```bash
docker build -f .docker/Dockerfile -t sauron:latest .
docker run -d --name sauron -p 3000:3000 -p 8080-8082:8080-8082 \
  -v /path/to/config.yaml:/app/config.yaml:ro sauron:latest
```

Probes: `/health` for liveness and `/ready` for readiness. `/ready` answers 503 until the first round of checks has finished (every internal node checked once; with only externals, the rings queried once), then 200 for good. It does not follow node health: replicas that share nodes would all leave the load balancer at once when the nodes fail, and clients would get connection errors instead of Sauron's fast `503 No available nodes`.

### Security

- The proxy ports are unauthenticated: keep them on private networks or behind a gateway.
- Use HTTPS for rings and TLS for gRPC to nodes outside a trusted network (`grpc_insecure: false`).
- External rings choose what they advertise: only list rings you trust.
- Keep `timeouts.proxy` bounded, and enable `rate_limit` on a status API that is reachable from outside.

## Troubleshooting

**A node is not getting traffic:** check `sauron_node_available` and `sauron_node_height` for it (it may be behind or failing a check), the logs for "Circuit breaker tripped", and `sauron_routing_selections_total` for its network.

**Requests fail with "No available nodes":** `sauron_routing_failures_total` gives the reason (`no_nodes`, `no_websocket_nodes`, `zero_height`); every node is unhealthy for that protocol or has no height yet.

**External endpoints unused:** compare `sauron_external_endpoints_{tracked,validated,working}`; they are only used when internals fall behind by more than the threshold. Check the ring's token and that it advertises endpoints.

**gRPC failures:** `grpc_insecure` must match the node (plaintext vs TLS); node addresses are `host:port` without a scheme. `sauron_proxy_errors_total{type="grpc"}` shows whether the status came from the node (`backend_status`), the client (`client_error`) or Sauron (`proxy_error`).

**Reload did not apply:** `sauron_config_reload_ok` is 0 and the log says which startup-only fields need a restart, or why the file was rejected.
