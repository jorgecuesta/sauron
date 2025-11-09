# Sauron

> Intelligent height-based routing proxy for Pocket Network nodes

**Sauron** monitors blockchain nodes across multiple deployments and automatically routes requests to the best available endpoint based on block height and latency. Deploy multiple instances for cross-region failover and distributed resilience.

> **Note:** Project name inspired by Tolkien's works. This is an open-source, non-commercial project with no affiliation to the Tolkien Estate.

---

## The Problem

Running off-chain actors (RelayMiners, Gateways, Indexers, Wallets) against Pocket Network nodes requires continuous availability. Any node can experience issues at any time—network partitions, sync delays, resource constraints, or maintenance windows.

**Without intelligent routing:**
- If your node falls behind, you can't validate relays = zero rewards
- Node unavailability = failed relay production = lost productivity
- Stale node data = incorrect indexing = bad query results
- Wrong block height = rejected transactions = failed operations

**With Sauron:**
- Automatic routing to the highest block height node
- Instant failover when nodes fall behind or go offline
- Multi-region support with distributed endpoint discovery
- Zero manual intervention required

---

## Prerequisites

- Go 1.21 or higher
- Access to Pocket Network nodes (validators or full nodes)
- Available ports: 3000 (status API), 8080 (API proxy), 8081 (RPC proxy), 8082 (gRPC proxy)

---

## Quick Start

### 1. Install

```bash
git clone https://github.com/jorgecuesta/sauron
cd sauron
make build
```

### 2. Configure

Create `config.yaml`:

```yaml
listen: ":3000"

timeouts:
  health_check: 5s
  proxy: 60s

networks:
  - name: "pocket"
    api_listen: ":8080"
    rpc_listen: ":8081"
    grpc_listen: ":8082"

internals:
  - name: node-1
    api: "https://node1.example.com"
    rpc: "https://node1.example.com:26657"
    grpc: "node1.example.com:9090"
    network: "pocket"
  - name: node-2
    api: "https://node2.example.com"
    rpc: "https://node2.example.com:26657"
    grpc: "node2.example.com:9090"
    network: "pocket"
```

### 3. Run

```bash
./sauron -config config.yaml
```

### 4. Use It

Point your off-chain actors to Sauron's proxy ports:

```yaml
# RelayMiner, Gateway, Indexer, etc.
node_api_url: "http://localhost:8080"
node_rpc_url: "http://localhost:8081"
node_grpc_url: "localhost:8082"
```

Sauron will automatically route all requests to the best available node.

---

## How It Works

```
┌──────────────┐         ┌──────────────┐         ┌─────────────────┐
│   Clients    │────────▶│    Sauron    │────────▶│  Best Node      │
│ (RelayMiner, │         │   (Proxy)    │         │ (height 508K)   │
│  Gateway,    │         │              │         │                 │
│  Indexer)    │         │  Monitors:   │         │  Alternatives:  │
└──────────────┘         │  • Height    │         │  • Node A: 508K │
                         │  • Latency   │         │  • Node B: 507K │
                         │  • Health    │         │  • Ext C: 508K  │
                         └──────────────┘         └─────────────────┘
```

**Selection Algorithm:**
1. Find the highest block height across all nodes
2. Among nodes at max height, select the one with lowest latency
3. Instant failover if the selected node fails

**Health Checks (every 5 seconds):**
- API: `GET /cosmos/base/tendermint/v1beta1/blocks/latest`
- RPC: `GET /status`
- gRPC: `cosmos.base.tendermint.v1beta1.Service/GetLatestBlock`

---

## Features

- ✅ **Height-based routing**: Always use the node with the highest block height
- ✅ **Latency optimization**: Among nodes at max height, pick the fastest
- ✅ **Multi-protocol**: API (HTTP), RPC (HTTP), gRPC support
- ✅ **External discovery**: Query other Sauron deployments for additional endpoints
- ✅ **Hot reload**: Update configuration without restart (SIGHUP)
- ✅ **Authentication**: Token-based access control
- ✅ **Observability**: Full Prometheus metrics
- ✅ **Docker support**: Production-ready containerization

---

## Documentation

- **[HOW_THIS_WORKS.md](HOW_THIS_WORKS.md)** - Architecture, components, and technical details
- **[.docker/TESTING.md](.docker/TESTING.md)** - Testing guide and validation

---

## Testing

```bash
# Run validation tests
make docker-test

# Run advanced feature tests
make docker-test-advanced

# Manual testing
make docker-up
curl http://localhost:3000/health
make docker-down
```

---

## Development

```bash
make build    # Build binary
make fmt      # Format code
make lint     # Run linters
make clean    # Clean build artifacts
```

---

## License

MIT License - see [LICENSE](LICENSE) file for details

---

## Contributing

Contributions welcome! Please open an issue or PR.

- Architecture details: [HOW_THIS_WORKS.md](HOW_THIS_WORKS.md)
- Testing guide: [.docker/TESTING.md](.docker/TESTING.md)
