# Objectives — open findings and work queue

The list of what to work on next. A finding is recorded only once it is here.
Mark each claim as **measured**, **verified in code**, or **not verified**.

## 1. gRPC proxy drops response metadata and rewrites backend errors — FIXED ON BRANCH `fix/grpc-response-metadata`, NOT PUSHED

- **Status (2026-10-22):** committed on the branch. Every fix has a test that failed before it, except the second-SIGTERM escape, whose test would need a full `Server` in a subprocess. Gates: build, `go test -race ./...` (5 runs), vet, `make lint`, gofmt, `go mod tidy`. L3: `make docker-test` 23/23 and `make docker-test-advanced` 27/27.
  - Local e2e (local sauron → seed-one port-forward), measured: `x-cosmos-block-height` arrives through sauron. The error `NotFound: supplier with operator address: "pokt1doesnotexist": supplier not found` is byte-identical to the direct call.
  - Pending: the live check on beta after a deploy (`grpcurl -v` shows the header).
- **Also fixed on the same branch** (found by code reviews and L3):
  - **gRPC proxy:**
    - The call ends on the backend's status even when the client never half-closed; this was a deadlock.
    - A client-side failure no longer hangs on a send blocked by flow control.
    - Call outcomes are classified by origin (`backend_status`, `client_error`, `proxy_error`) for logs and the `ProxyErrors` reason, and only `proxy_error` logs at Error.
    - Proxy-side errors keep a clean gRPC message.
  - **SIGHUP:**
    - It used to kill the process (no handler; measured restart count 0→1, then "No height data"). It now reloads the config (measured 0→0).
    - The file-watcher reload never worked for configs with durations, because of a missing decode hook. Fixed.
    - Concurrent reloads end on the newest file.
    - Restart-only changes are rejected with `ErrRestartRequired`. Networks and endpoints are matched by name and protocol, so reordering the YAML is not treated as a restart-only change.
    - New gauge `sauron_config_reload_ok`.
    - A second SIGTERM/SIGINT exits immediately during graceful shutdown.
  - **WebSocket:** a client dropping its connection is `client_disconnect` and no longer counts against the node's circuit breaker.
  - **Tests:** the L3 suites check V2 endpoint names. The flaky WebSocket happy-path test was fixed: up to 29 of 40 runs failed on `main` under load.

- **Measured (2026-10-21):** seed-one direct over gRPC returns `x-cosmos-block-height`. The same call through `sauron-grpc.{beta.,}infra.pocket.network:443` returns no such header and empty trailers.
- **Verified in code:** `proxy/grpc_proxy.go` never calls `clientStream.Header()`/`SetHeader` or `clientStream.Trailer()`/`SetTrailer`. Backend errors are returned as `fmt.Errorf("recv from backend: %w")`. grpc-go v1.68 `status.FromError` keeps the code but replaces the message (`status/status.go:112-124`).
- **Impact (relay miner, read in its code, not measured):** without the header the miner cannot cache query answers as final and cannot treat "claim not found" as definitive.
- **Done when:** L1 tests show the header, the trailer, a trailers-only error and the backend status (code, message, details) all arrive as in a direct call. A client cancelling mid-stream is race-clean. `grpcurl -v` through beta shows `x-cosmos-block-height`.

## 2. Height skew: RPC and gRPC route to nodes at different heights

- **Measured on beta:** 240 paired samples over 11 blocks. In 6 blocks, RPC and gRPC were 1 block apart for ≤1.6s.
- **Measured on seed-one direct:** `/status` (blockstore) runs ahead of the app-committed height (query header `x-cosmos-block-height`). This happened in 2 of 14 blocks, once for ~1.3s and once for ≥9.6s. Routing alone cannot close this gap.
- **Verified in source by the relay-miner session** (pokt-network/cometbft v0.38.19-0.20261013205308):
  - `consensus/state.go`: `SaveBlock` (:1730) runs before `ApplyVerifiedBlock` (:1770).
  - `state/execution.go`: the app `Commit` (:321) runs before `fireEvents` (:351).
  - So `/status` and `/block` report H before the app commits it, and NewBlock fires only after.
- **Verified in code:** the height-check cron is hardcoded to `*/30` (`checker/v2_scheduler.go:87`). `height_check.interval` (`HeightCheckInterval()`) has no callers. The cosmos height check reads `/status` (`adapter/cosmos/factory.go:32`).
- **Plan, in order:**
  - 2a. Switch the cosmos height check to `/abci_info` `.result.response.last_block_height`, which is the app-committed height.
  - Build a causal probe and measure. For each NewBlock H seen through the sauron RPC websocket, send an immediate gRPC query and read the response header. Run the same probe against a direct node as a control.
  - 2b, only if the measurement justifies it. Honour the interval with a height-only loop, separate from the health checks, with:
    - one check per node at a time;
    - stored heights that never go down;
    - the `s.timeout` write race fixed.
  - 2c is relay-miner side and coordinated through Jorge. The candidate is moving the miner's poll from `Status` to `ABCIInfo.LastBlockHeight` (`client/block_subscriber.go:464`).
- **Done when:** ≥99.5% of blocks pass the causal probe (gRPC height ≥ H) over ≥200 blocks on beta. A miner load run of the same size as the 16-case baseline has 0 out-of-window rejections, with the denominator recorded.

## 3. Pre-existing issues found by the 2026-10-22 config audit (not yet fixed)

All verified in code by a read-only audit; none measured at runtime.

- **Data race on `MultiChainScheduler.timeout` — FIXED (2026-10-22).** Several cron jobs wrote it without a lock. After the Go 1.27.3 bump, `-race` caught it in the checker tests. It is now an `atomic.Int64`.
- **Removed or renamed internal nodes are never forgotten.** Height and health stores have no delete (`storage/heights.go`, `storage/health.go`), so the selector keeps picking the old name. HTTP then skips it and uses up a retry (`proxy/http_proxy.go:139-146`); gRPC returns `Internal` (`proxy/grpc_proxy.go`).
- **A node URL change leaks the old pooled gRPC connection** until shutdown (`GRPCProxy.connPool`, `ExternalChecker.grpcConnections`).
- **Removed externals and rings stay routable.** `ExternalEndpointStore` has no delete.
- **Config fields that are accepted but read by nothing:** `height_check.interval` (the cron is fixed at 30s; item 2b), `archival.check_interval`, `sync.check_interval`, `circuit_breaker.grpc_codes`.

## 4. The test suite does not prove the service works — PLANNED, highest priority before any release

Context: v2.0.0-beta.2 shipped with every RPC WebSocket upgrade rejected (503). Mainnet ran it from about 19:25 to 19:31Z on 2026-10-22, then was rolled back to 1.5.0. Jorge's requirement: tests must be real, live in the repo, run with `make` by anyone, and run in CI.

**Measured on 2026-10-22, branch fix/websocket-availability.**

Mutation testing (gremlins), test efficacy and surviving mutants per package:
- proxy 69.6% (35); selector 79.3% (12); checker 83.8% (11); storage 82.9% (6); status 85.3% (5); config 94.9% (5); jsonpath 91.9% (3).
- server covers only 21% of its mutants.
- adapter, adapter/cosmos and urlutil are at 100%.

**Verified in code:**
- CI runs `go test -v ./...` without `-race`, so race-only tests check nothing in CI.
- L3 is not in CI.
- L3 has about 11 checks that always pass (`print_pass` in failure branches). TEST 16 greps a metric that does not exist, `sauron_proxy_requests_total`.

**Coverage audit (read-only agent, spot-checked):**
- gRPC is the only protocol with strong behavioural tests.
- RPC GET and JSON-RPC POST have no tests at any level.
- No protocol has a failover-through-the-proxy test.
- Proxy L2 tests set store state by hand instead of running the scheduler.

**Production bugs the tests hid** (verified in code; impact not measured):
- The gRPC proxy never calls its circuit breaker, and its health check only checks that the transport is READY. A node failing every query stays in rotation.
- A dead gRPC backend returns `Internal "failed to create stream"` instead of `Unavailable` (`proxy/grpc_proxy.go:303`).
- An HTTP client cancel counts as a backend failure (errRetriable → RecordError and a retry).
- WebSocket:
  - the selector ignores `WebSocketAvailable`;
  - a backend that answers the upgrade with something other than 101 is recorded as a breaker success;
  - the copy loops have no idle deadline.
- JSON-RPC bodies over 10 MB are silently truncated and still forwarded.
- **External ring outage never marks its endpoints failed** (measured 2026-10-22 with the honest advanced L3 suite). The secondary was stopped for 120s, and the primary still reported 3 working external endpoints, an error count of 0 and no recoveries. A dead ring's validated endpoints stay routable. Production uses no externals today, so impact is low. The honest TESTs 6, 9 and 28 fail on this until it is fixed.

**Proposed order:** CI that really tests → honest L3 with deterministic fake backends in CI → L2 protocol matrix with production wiring → fix the bugs above, each with a failing test first → kill the surviving mutants in critical packages, with `make mutation` as a nightly CI job → only then release v2.0.0-beta.3.
