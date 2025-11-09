# How Sauron Works

Sauron is an intelligent routing proxy for Pocket Network nodes that selects the best available endpoint based on height and latency.

## Architecture

```
┌─────────────┐         ┌─────────────┐         ┌──────────────┐
│   Clients   │────────▶│   Sauron    │────────▶│   Backends   │
│  (API/RPC/  │         │   (Proxy)   │         │  (Internal + │
│   gRPC)     │         │             │         │   External)  │
└─────────────┘         └─────────────┘         └──────────────┘
                               │
                               ▼
                        ┌─────────────┐
                        │  Selector   │
                        │ (Height →   │
                        │  Latency)   │
                        └─────────────┘
                               │
                    ┌──────────┴──────────┐
                    ▼                     ▼
             ┌─────────────┐       ┌─────────────┐
             │  Internal   │       │  External   │
             │   Nodes     │       │ Endpoints   │
             └─────────────┘       └─────────────┘
```

## Core Components

### 1. Health Checkers (`checker/`)
Run periodic health checks on internal nodes:
- **API Checker**: `GET /cosmos/base/tendermint/v1beta1/blocks/latest`
- **RPC Checker**: `GET /status`
- **gRPC Checker**: `cosmos.base.tendermint.v1beta1.Service/GetLatestBlock`

Each checker extracts height and measures latency (10-sample moving average).

### 2. External Endpoint Discovery (`checker/external.go`)
Queries other Sauron rings via `/status` API to:
1. Discover advertised endpoints (API, RPC, gRPC)
2. Validate connectivity
3. Track health and performance

External endpoints go through states: `ADVERTISED → VALIDATED → [WORKING|FAILED] → RECOVERED`

### 3. Selector (`selector/`)
Chooses the best endpoint using this algorithm:
1. **Find max height** among all candidates (internal + external)
2. **Filter** to only nodes at max height
3. **Select lowest latency** among filtered nodes

External endpoints compete equally with internal nodes.

### 4. Proxies (`proxy/`)
- **HTTP Proxy**: Handles API (port 8080) and RPC (port 8081) requests
- **gRPC Proxy**: Handles gRPC requests (port 8082) with transparent proxying

Each proxy:
1. Calls selector for best endpoint
2. Forwards request to selected backend
3. Records metrics and errors
4. Tracks 5xx errors for external endpoint health

### 5. Storage (`storage/`)
- **HeightStore**: Tracks internal node heights and latencies
- **ExternalEndpointStore**: Tracks external endpoint states and metrics

### 6. Metrics (`metrics/`)
Prometheus metrics for monitoring:
- Node heights and latencies
- Routing decisions
- Proxy request counts and durations
- External endpoint validation states
- Error counts and recovery events

## Request Flow

1. Client sends request to Sauron proxy port
2. Proxy calls Selector with (network, endpoint_type)
3. Selector evaluates all candidates (internal + external)
4. Selector returns best endpoint based on height→latency
5. Proxy forwards request to selected backend
6. Response returned to client
7. Metrics recorded

## Health Check Cycle

Every 5 seconds:
1. Checkers query all internal nodes
2. Extract height and measure latency
3. Update HeightStore with metrics
4. External checker queries external rings
5. Validate advertised endpoints
6. Update ExternalEndpointStore
7. Failed endpoints retried every 10s

## External Endpoint Integration

External endpoints from other Sauron rings are:
- Discovered via `/status` API
- Validated for connectivity
- Tracked in separate store
- Added to selector candidate pool with `ext:` prefix
- Selected using same height→latency algorithm as internal nodes

This enables distributed failover across multiple Sauron deployments.

## Configuration

### Basic Configuration

```yaml
# Status API listen address
listen: ":3000"

# Enable protocol support
api: true
rpc: true
grpc: true

# Timeouts
timeouts:
  health_check: 5s  # Health check interval
  proxy: 60s        # Proxy request timeout

# Network proxies
networks:
  - name: "pocket"
    api_listen: ":8080"     # API proxy port
    rpc_listen: ":8081"     # RPC proxy port
    grpc_listen: ":8082"    # gRPC proxy port
    grpc_insecure: true     # Use plaintext gRPC (for testing)

# Internal nodes to monitor
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

# External Sauron rings to query
externals:
  - name: partner-sauron
    token: "shared-secret"
    rings:
      - "https://partner.example.com:3000"

# API authentication
auth: true
users:
  - name: relayminer-1
    token: "secret-token-1"
    api: true    # Can use API proxy
    rpc: true    # Can use RPC proxy
    grpc: true   # Can use gRPC proxy
```

### Hot Reload

Update configuration without restarting:

```bash
# Edit config.yaml
vim config.yaml

# Send SIGHUP signal to reload
kill -HUP <sauron-pid>
```

Changes are applied immediately without dropping active connections.

### Authentication

Enable token-based authentication:

```yaml
auth: true

users:
  - name: service-1
    token: "secret-token-1"
    api: true    # API access granted
    rpc: false   # RPC access denied
    grpc: true   # gRPC access granted
```

Clients must include the token in requests:

```bash
# API request with auth
curl -H "Authorization: Bearer secret-token-1" \
  http://localhost:8080/cosmos/base/tendermint/v1beta1/blocks/latest

# RPC request with auth
curl -H "Authorization: Bearer secret-token-1" \
  http://localhost:8081/status
```

### External Discovery

Configure Sauron to discover endpoints from other deployments:

```yaml
externals:
  - name: other-deployment
    token: "shared-secret"
    rings:
      - "https://other.sauron.com:3000"
```

Sauron will:
1. Query `https://other.sauron.com:3000/pocket/status`
2. Discover advertised endpoints (API, RPC, gRPC)
3. Validate connectivity to each endpoint
4. Add working endpoints to routing pool
5. Monitor health and auto-recover failed endpoints

## Monitoring

### Prometheus Metrics

Access metrics at `:3000/metrics`:

#### Node Metrics

```
# Current block height per node
sauron_node_height{network="pocket",node="node-1",type="api",source="internal"} 508365

# Health check latency in seconds
sauron_node_latency_seconds{network="pocket",node="node-1",type="api"} 0.325

# Node availability (1=up, 0=down)
sauron_node_available{network="pocket",node="node-1",type="api"} 1
```

#### Routing Metrics

```
# Routing decisions made (by node and reason)
sauron_node_selections_total{network="pocket",type="api",node="node-1",reason="height"} 142

# Number of candidates considered per routing decision
sauron_routing_alternatives_considered{network="pocket",type="api"} 3
```

#### Proxy Metrics

```
# Total requests proxied
sauron_proxy_requests_total{network="pocket",node="node-1",type="api",method="GET"} 1523

# Request duration histogram
sauron_proxy_request_duration_seconds_bucket{network="pocket",node="node-1",type="api",status="200",le="0.1"} 1420

# Proxy errors
sauron_proxy_errors_total{network="pocket",node="node-1",type="api",status="503",reason="backend_unavailable"} 3
```

#### External Endpoint Metrics

```
# Endpoints discovered from external rings
sauron_external_endpoints_tracked{network="pocket",type="api",external="partner-sauron"} 3

# Endpoints successfully validated
sauron_external_endpoints_validated{network="pocket",type="api",external="partner-sauron"} 2

# Failed endpoints that recovered
sauron_external_endpoint_recoveries_total{network="pocket",type="api",external="partner-sauron"} 1
```

### Grafana Dashboard

Example PromQL queries:

```promql
# Average node height by type
avg by (type) (sauron_node_height)

# Nodes currently at max height
sauron_node_height == on (network, type) group_left max by (network, type) (sauron_node_height)

# Request rate per node
rate(sauron_proxy_requests_total[5m])

# P95 latency
histogram_quantile(0.95, rate(sauron_proxy_request_duration_seconds_bucket[5m]))

# External endpoint health
sauron_external_endpoints_validated / sauron_external_endpoints_tracked
```

## Production Deployment

### Recommended Setup

Deploy Sauron in multiple regions with cross-region failover:

```
┌─────────────┐           ┌─────────────┐           ┌─────────────┐
│  Sauron A   │ ◀────────▶│  Sauron B   │◀─────────▶│  Sauron C   │
│  (Region 1) │  External │  (Region 2) │  External │  (Region 3) │
│             │  Discovery│             │  Discovery│             │
└─────────────┘           └─────────────┘           └─────────────┘
       │                         │                         │
       ▼                         ▼                         ▼
  Internal Nodes           Internal Nodes           Internal Nodes
  (Validators,             (Validators,             (Validators,
   Full Nodes)              Full Nodes)              Full Nodes)
```

**Benefits:**
- Each region monitors local nodes (low latency health checks)
- Cross-region failover via external discovery
- Distributed observability
- Independent deployments (no single point of failure)

**Configuration for Region A:**

```yaml
listen: ":3000"

networks:
  - name: "pocket"
    api_listen: ":8080"
    rpc_listen: ":8081"
    grpc_listen: ":8082"

# Local nodes in Region 1
internals:
  - name: region1-node-1
    api: "https://node1.region1.internal"
    rpc: "https://node1.region1.internal:26657"
    grpc: "node1.region1.internal:9090"
    network: "pocket"
  - name: region1-node-2
    api: "https://node2.region1.internal"
    rpc: "https://node2.region1.internal:26657"
    grpc: "node2.region1.internal:9090"
    network: "pocket"

# Other Sauron deployments
externals:
  - name: region2-sauron
    token: "shared-secret-region2"
    rings:
      - "https://sauron.region2.internal:3000"
  - name: region3-sauron
    token: "shared-secret-region3"
    rings:
      - "https://sauron.region3.internal:3000"
```

### Security Considerations

1. **Token Security**
   - Use strong, unique tokens for each user/service
   - Rotate tokens periodically
   - Store tokens securely (environment variables, secrets manager)

2. **Network Isolation**
   - Run Sauron in private network when possible
   - Use VPN or private peering between regions
   - Restrict external ring discovery to trusted deployments

3. **TLS**
   - Use HTTPS for external ring discovery
   - Enable TLS for gRPC in production (`grpc_insecure: false`)
   - Use valid certificates (not self-signed)

4. **Firewall Rules**
   - Limit access to status API (`:3000`) to monitoring systems only
   - Restrict proxy ports (`:8080`, `:8081`, `:8082`) to authorized clients
   - Use network policies or security groups

5. **Resource Limits**
   - Set reasonable `proxy` timeouts to prevent hanging connections
   - Monitor memory usage and set limits
   - Use rate limiting if needed

### Docker Deployment

See `.docker/` directory for production-ready containerization:

```bash
# Build image
docker build -f .docker/Dockerfile -t sauron:latest .

# Run container
docker run -d \
  --name sauron \
  -p 3000:3000 \
  -p 8080:8080 \
  -p 8081:8081 \
  -p 8082:8082 \
  -v /path/to/config.yaml:/app/config.yaml:ro \
  sauron:latest
```

### Health Checks

Kubernetes/Docker health check:

```yaml
livenessProbe:
  httpGet:
    path: /health
    port: 3000
  initialDelaySeconds: 10
  periodSeconds: 10

readinessProbe:
  httpGet:
    path: /health
    port: 3000
  initialDelaySeconds: 5
  periodSeconds: 5
```

## Troubleshooting

### Node Selection Issues

**Problem:** Sauron not routing to expected node

**Debug:**
1. Check selector metrics: `sauron_node_selections_total`
2. Verify node heights: `sauron_node_height`
3. Check node availability: `sauron_node_available`
4. Review logs for selection decisions

**Common causes:**
- Node actually has lower height than expected
- Node health checks failing
- Higher latency than alternative nodes

### External Endpoint Discovery

**Problem:** External endpoints not discovered

**Debug:**
1. Check tracked endpoints: `sauron_external_endpoints_tracked`
2. Verify validated endpoints: `sauron_external_endpoints_validated`
3. Test manual access to external ring status API
4. Check authentication token

**Common causes:**
- Network connectivity issues
- Incorrect token
- External ring not advertising endpoints
- Firewall blocking access

### gRPC Issues

**Problem:** gRPC requests failing

**Debug:**
1. Check `grpc_insecure` setting matches backend
2. Test with `grpcurl` directly to backend
3. Verify gRPC endpoint format (no `http://` prefix)
4. Check TLS certificate validity

**Common causes:**
- TLS handshake failures (use `grpc_insecure: true` for testing)
- Incorrect endpoint format
- Backend not supporting gRPC reflection

### Performance Issues

**Problem:** High latency or slow responses

**Debug:**
1. Check proxy duration metrics: `sauron_proxy_request_duration_seconds`
2. Compare to health check latency: `sauron_node_latency_seconds`
3. Review backend performance
4. Check network conditions

**Common causes:**
- Backend node overloaded
- Network congestion
- Too many concurrent requests
- Proxy timeout too low
