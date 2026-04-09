# Sauron

## What This Is

Sauron is an intelligent routing proxy for Pocket Network blockchain nodes (Cosmos SDK). It monitors node block heights via periodic health checks and routes client requests (API/RPC/gRPC) to the best available node using a height-based selection algorithm with round-robin load distribution.

**Key use case**: Off-chain actors (RelayMiners, Gateways, Indexers) point at Sauron instead of individual nodes. Sauron handles failover, load distribution, and cross-region discovery automatically.

## How to Run

```bash
# Build
make build

# Run with config
./bin/sauron -config config.yaml

# Or with default config.yaml in current directory
./bin/sauron
```

**Ports** (configurable per network):
- `:3000` — Status API, health checks, Prometheus metrics
- `:8080` — API proxy (Cosmos REST)
- `:8081` — RPC proxy (Tendermint)
- `:8082` — gRPC proxy (Cosmos gRPC)

## How to Test

```bash
make test             # go test -v ./...
make fmt              # gofmt + goimports
make lint             # golangci-lint (falls back to go vet)
go test -race ./...   # Race detector — always run before committing
go test -count=3 ./...  # Flaky test check
```

**Test conventions**:
- stdlib `testing` only — no testify, gomock, or external frameworks
- Temp YAML files + `zap.NewNop()` + in-memory stores for setup
- `t.Helper()` for helpers, `t.Parallel()` where safe
- Tests exist in: `selector/`, `config/`, `storage/`, `status/`, `proxy/`, `checker/`, `internal/urlutil/`, `adapter/`, `internal/jsonpath/`

### Test Quality Requirements (NON-NEGOTIABLE)

Every test file MUST cover all of these categories. If a category is missing, the tests are incomplete:

1. **Happy paths**: Every public function's primary use case, with realistic data matching production inputs (e.g. actual Cosmos RPC responses, real EVM hex heights, real GraphQL payloads).

2. **Error/wrong paths — equal priority to happy paths**:
   - Invalid input (malformed JSON, empty input, nil values)
   - Missing data (key not found, empty responses, null fields)
   - Wrong types (boolean where int expected, object where string expected)
   - Network failures (connection refused, context canceled, timeout)
   - HTTP error codes (500, 503, 404)

3. **Edge cases — as many as reasonable**:
   - Zero values, negative values, empty strings, empty collections
   - Boundary values (max int64, overflow, very large inputs)
   - Unicode, special characters in keys/values
   - Trailing/leading whitespace
   - Empty but valid structures (`{}`, `[]`)

4. **Field-level verification**: Do NOT just check that a function returns "something". Verify specific field values. If a factory produces a config with `URLPath`, `Method`, `Headers`, `ResponsePath` — check ALL of them, not just one.

5. **Test helper correctness**: If you write a comparison helper (like `jsonEqual`), it must handle ALL types the function can return — maps, slices, nil, primitives. Panics in test helpers are bugs.

6. **Error type verification**: When functions return sentinel errors (`ErrNotFound`, `ErrType`), use `errors.Is()` to verify the correct error type, not just that an error occurred.

7. **No magic strings in test logic**: Do NOT use `if tt.name == "special case"` in the test loop. Use struct fields (`wantAnyErr bool`) to control test behavior.

8. **Integration-like tests**: For HTTP clients/engines, test with `httptest.NewServer` using realistic response bodies. For context-aware code, test with canceled contexts.

## Config Format

```yaml
listen: ":3000"       # Status API
api: true             # Enable API proxying
rpc: true             # Enable RPC proxying
grpc: true            # Enable gRPC proxying
auth: false           # Bearer token auth on status API
external_failover_threshold: 2  # Blocks behind before using externals

timeouts:
  health_check: 5s
  proxy: 60s

rate_limit:
  enabled: true
  requests_per_second: 100
  burst: 200
  trust_proxy: true   # Trust X-Forwarded-For headers

redis:
  enabled: false
  uri: "redis://localhost:6379"

networks:
  - name: pocket
    api: "https://sauron-api.example.com"     # Advertised URL (for external rings)
    rpc: "https://sauron-rpc.example.com"
    grpc: "sauron-grpc.example.com:9090"
    api_listen: ":8080"
    rpc_listen: ":8081"
    grpc_listen: ":8082"

internals:
  - name: node-1
    network: pocket
    api: "http://node1:1317"
    rpc: "http://node1:26657"
    grpc: "node1:9090"
    grpc_insecure: true

externals:
  - name: partner
    token: "shared-secret"
    rings:
      - "https://partner-sauron:3000"

users:
  - name: relayminer-1
    token: "secret-token"
    api: true
    rpc: true
    grpc: false
```

Hot reload: edit config.yaml → `kill -HUP <pid>` or file watcher auto-detects.

## Architecture

```
Client → Proxy (HTTP/gRPC) → Selector → Best Node
                                ↑
                    Checker (periodic health) → HeightStore
                    ExternalChecker → ExternalEndpointStore
```

### Package Map

| Package | Purpose | Key types |
|---------|---------|-----------|
| `server/` | Orchestrator, lifecycle, graceful shutdown | `Server` |
| `proxy/` | HTTP reverse proxy + gRPC transparent proxy | `HTTPProxy`, `GRPCProxy` |
| `checker/` | Health checks: API, RPC, gRPC, external rings, WebSocket | `Scheduler`, `APIChecker`, `RPCChecker`, `GRPCChecker`, `ExternalChecker` |
| `selector/` | Node selection: max height → sorted round-robin | `Selector` |
| `storage/` | Height store, external endpoints, Redis cache | `HeightStore`, `ExternalEndpointStore`, `Cache` |
| `config/` | Viper loader, validator, atomic reads | `Loader`, `Config` |
| `status/` | Status API, auth middleware, rate limiter | `Handler`, `RateLimiter` |
| `metrics/` | Prometheus metric declarations | package-level vars |
| `internal/urlutil/` | URL normalization utilities | `NormalizeURL`, `TrimTrailingSlash` |
| `checker/common.go` | Shared checker helpers | `recordCheckError`, `recordCheckSuccess` |
| `checker/websocket.go` | Shared WebSocket validation | `CheckWebSocket` |

### Request Flow

1. Client hits proxy port (8080/8081/8082)
2. `Selector.GetBestNode(network, type)` picks best node:
   - Gets all internal nodes from `HeightStore`
   - Optionally adds externals if ahead by threshold
   - Filters to max height, sorts by name, round-robin selects
3. Proxy forwards to selected backend
4. Metrics recorded, errors tracked

### Health Check Cycle (cron-based via `robfig/cron`)

- Every 30s: check all internal nodes (API path, RPC /status, gRPC ABCIQuery)
- Every 10s: query external Sauron rings for endpoint discovery
- Every 10s: recover failed external endpoints

## Code Conventions

### Concurrency Patterns
- **Config**: `atomic.Pointer[Config]` — `loader.Get()` returns `*Config`, zero alloc, treat as immutable
- **Height store**: `xsync.Map` — lock-free concurrent access
- **gRPC connections**: `xsync.Map.LoadOrCompute` — prevents TOCTOU races
- **Round-robin**: sorted candidates + per-`network:type` atomic counters via `sync.Map`
- **Shutdown**: error channels for startup failures, `stopCh` channels for goroutine termination

### Logging Rules
- Per-request logs: `Debug` level only (never Info on hot path)
- State changes (failover, config reload, endpoint recovery): `Info`
- Errors: `Error`
- Never `logger.Fatal` in goroutines — use error channel propagation

### Metrics Rules
- No high-cardinality labels (no URLs, no full gRPC method paths as labels)
- Node source label: `"internal"` / `"external"` (not node names in RoutingSelections)
- gRPC methods: normalized to service name only (`pocket.session.Query`, not `/pocket.session.Query/GetSession`)
- Delete unused metrics immediately — no dead declarations

### Error Handling
- Always `Close()` replaced connections before overwriting pool entries
- Startup errors propagated via buffered `errCh`, not `os.Exit`
- Rate limiter uses `stopCh` channel (Go tickers don't close their channel on `Stop()`)

### DRY
- URL normalization: use `internal/urlutil` (not inline)
- Checker helpers: use `checker/common.go` (`recordCheckError`, `recordCheckSuccess`, `newCheckerHTTPClient`)
- WebSocket validation: use `checker/websocket.go` (`CheckWebSocket`)
- Network lookup: use `config.FindNetwork(name)` (not manual loop)
- gRPC message sizes: use `GRPCProxy.getMessageSizeLimits()`

## Build & CI

```bash
make build    # Build binary to bin/sauron
make test     # go test -v ./...
make fmt      # gofmt -s + goimports
make lint     # golangci-lint run (falls back to go vet)
make clean    # Remove binaries
```

CI triggers on git tags (`v*`). Pipeline: test → lint → fmt-check → cross-platform build (linux/darwin, amd64/arm64) → Docker push to ghcr.io.

## Production Environment

Deployed in Kubernetes (`pnf` context, `mainnet` namespace) with:
- 3 fullnodes: `seed-one`, `seed-two`, `seed-three` (Cosmos SDK / pocketd)
- Prometheus + Grafana in `monitoring` namespace
- Alloy for log collection
- ArgoCD for deployment

## Behavioral Standards

### Be Critical, Not Complacent
- Never say "looks good" without evidence. If code passes, show the proof (test output, build output, metric data).
- Challenge assumptions. If the user or another agent claims something, verify it independently before acting on it.
- When reviewing code, actively look for problems. A review that finds nothing is suspicious — re-examine.
- If you disagree with an approach, say so with reasoning. Do not silently comply with a bad plan.

### Demand Clarity
- If a task is ambiguous, ASK before implementing. Do not guess intent.
- If you lack context about why a change is needed, ask for the motivation. Understanding "why" prevents wrong solutions.
- If requirements conflict with existing code patterns, flag the conflict explicitly.
- Never assume "the user probably means X" — confirm it.

### Quality Gates (Mandatory for Every Change)
Every code change must pass ALL of these before it is considered done:

1. **Build**: `go build ./...` — zero errors
2. **Tests**: `go test ./...` — all pass
3. **Race detector**: `go test -race ./...` — no races
4. **Vet**: `go vet ./...` — no issues
5. **Lint**: `make lint` (golangci-lint) — no issues. This catches errcheck, staticcheck, unused, and other rules that go vet misses. MUST run before pushing.
6. **Format**: `gofmt -l .` — no files listed
7. **Review**: Re-read the diff. Check for:
   - Unused imports or variables
   - Hardcoded values that should be configurable
   - Missing error handling
   - Concurrency issues (shared state without sync)
   - DRY violations (duplicated logic)
   - Log levels (no Info on hot paths)
   - Metric cardinality (no unbounded labels)

If any gate fails, fix it before reporting completion. Do NOT report "done" with known failures.

**No "pre-existing" excuses.** If a quality gate fails, fix it. Do not dismiss failures as "pre-existing" or "not related to my changes." The previous release passed all CI checks. If something fails now, either your change broke it or the environment drifted — either way, diagnose and fix it before moving on.

### Evidence-Based Reporting
- When reporting a bug: show the file, line number, and why it's wrong.
- When reporting a fix: show the before/after and the test that proves it.
- When reporting metrics: show the actual Prometheus query and its output.
- When reporting "no issues found": explain what you checked and how.

## Agent Model Strategy

When spawning agents for this project, use tiered models to conserve tokens:

| Task Type | Model | When |
|-----------|-------|------|
| Code execution (write code, tests, refactoring) | `model: "sonnet"` | Clear specs, implementation work |
| Exploration and research | `subagent_type: "Explore"` | Codebase search, architecture questions |
| Architectural decisions, audits, planning | `model: "opus"` | Nuanced judgment needed |
| Simple mechanical tasks (rename, delete, log levels) | `model: "haiku"` | Fastest, cheapest |

**Examples**:
```
Agent(description="Write storage tests", model="sonnet", ...)
Agent(description="Audit error handling", model="opus", ...)
Agent(description="Remove unused exports", model="haiku", ...)
Agent(description="Find gRPC usage patterns", subagent_type="Explore", ...)
```

**Key rule**: Do NOT use Opus for implementation tasks with clear specifications. Sonnet is sufficient and 5x more efficient. Reserve Opus for decisions that require weighing trade-offs.

## Parallel Agent Workflow Rules

These rules exist because past parallel refactoring introduced bugs through merge conflicts and oversight.

### 1. No Overlapping Files Between Parallel Agents
If two agents need to modify the same file, they CANNOT run in parallel. Either:
- Sequence them (A finishes, then B starts from A's output)
- Combine them into one agent with both tasks

**Why**: Worktrees branch from `main` at launch time. If Agent A and B both modify `proxy/grpc_proxy.go`, copying B's version overwrites A's changes. Manual re-application via sed/edit is error-prone and has caused lost fixes.

### 2. One Agent = One Focused Task
Do not give an agent 5+ tasks in one prompt. Max 2-3 closely related changes per agent. If a task list is long, split into sequential agents.

**Why**: Agents with long task lists deprioritize or skip items. Agent F was given 5 tasks and skipped H12 (gRPC dial DRY).

### 3. Agents Must Diff Against Original Behavior
When refactoring (replacing implementation, not just editing), the agent prompt MUST include: "Compare your new implementation against the original to verify no behavior was lost." Specifically:
- If replacing a stdlib function (like `NewSingleHostReverseProxy`), replicate ALL its behavior (query string merging, path joining, host header)
- If changing io.Copy sources/targets, verify buffer ownership

**Why**: Agent A wrote a new Director without replicating `RawQuery` merge logic from `httputil.NewSingleHostReverseProxy`. The WebSocket refactor copied from `clientConn` instead of `clientBuf`.

### 4. Post-Merge Verification Must Check Specific Fixes
After merging parallel agent work, do NOT just run `go build && go test`. Also:
- Grep for the exact pattern each fix was supposed to eliminate
- Verify the fix function/line exists in the merged code
- Run a targeted check: "does line X still contain the old buggy pattern?"

**Why**: `go test` passing does not mean the fix survived the merge. Tests may not cover the specific bug pattern.

### 5. `Stop()` / `Close()` / `Shutdown()` Must Be Idempotent
Any cleanup method must be safe to call multiple times. Use `sync.Once` for channel closes and resource cleanup.

**Why**: `ratelimit.Stop()` panics on double-close because `close(rl.stopCh)` has no guard.
