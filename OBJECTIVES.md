# Objectives — open findings and work queue

The list of what to work on next. A finding is recorded only once it is here.
Mark each claim as **measured**, **verified in code**, or **not verified**.

## 1. gRPC proxy drops response metadata and rewrites backend errors — FIXED LOCALLY, NOT COMMITTED

- **Status (2026-10-21):** fixed in `proxy/grpc_proxy.go`. Tests are in `proxy/grpc_proxy_metadata_test.go`; they failed before the fix and pass after it. All gates green: build, `go test -race ./...`, vet, `make lint`, gofmt.
  - L3: `make docker-test` 23/23. Its TEST 12 still checked the V1 type name `api`; I fixed it to `rest`. `make docker-test-advanced` 27/27. Neither suite proxies real gRPC, because the docker configs have no gRPC internals.
  - Local e2e (local sauron → seed-one port-forward), measured: `x-cosmos-block-height` arrives through sauron. The error `NotFound: supplier with operator address: "pokt1doesnotexist": supplier not found` is byte-identical to the direct call.
  - Pending: the live check on beta after a deploy.
  - Code-review follow-ups, fixed with failing tests written first:
    - The handler no longer hangs when the client stops reading and a client-side error occurs.
    - It no longer deadlocks when the backend ends before the client half-closes.
    - A relayed backend status no longer produces Error logs.
  - After those fixes, the L3 re-run was blocked: Tilt from another session holds host ports 3000 and 6379. Re-run both suites when they are free.

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
