# Deploying Sauron

Start here, pick a path, then follow its runbook from the first step.

## Words you will meet

- **Node**: a server that follows a blockchain and answers queries about it.
  For Pocket Network that is `pocketd`, serving Cosmos REST (port 1317),
  CometBFT RPC (26657, with WebSocket on `/websocket`) and Cosmos gRPC (9090).
- **Network**: one chain Sauron routes for, such as `pocket`. It has a `type`
  (`cosmos` for Pocket), its own listeners and its own nodes.
- **Endpoint**: a port Sauron listens on for one protocol of one network
  (`rest`, `rpc`, `grpc`; `jsonrpc` or any name for HTTP chains).
- **Internal node**: a node of yours, listed under `internals`, with one URL
  per protocol it serves.
- **External ring**: another Sauron deployment listed under `externals`. Its
  nodes are used only when yours fall more than `external_failover_threshold`
  blocks behind, or none is healthy.
- **Height**: the block number a node has reached. Sauron routes to the nodes
  at the highest height. For Pocket it is read from RPC `/abci_info`.
- **Beta and mainnet**: beta (chain id `pocket-lego-testnet`) is Pocket's test
  network, mainnet (`pocket`) the real one. Start on beta.
- **Stand-in node**: the example routes to the Pocket Network Foundation's
  public endpoint, `sauron-*.beta.infra.pocket.network`, so it starts before
  you have a node. It is not for real traffic.

## Choose a path

| | Docker Compose | Release binary | Kubernetes |
|---|---|---|---|
| Runbook | [DOCKER_COMPOSE.md](DOCKER_COMPOSE.md) | [below](#release-binary) | [notes below](#kubernetes); no manifests ship |
| What you get | the example in `examples/pocket/`, checked step by step | one static binary, your process manager | your manifests |
| Use when | first deployment, one host | a host without Docker | a cluster you already run |

When the human has not said, use Docker Compose: its runbook is the one run
step by step (step 8 with the stand-in's addresses, not a human's nodes).

## Versions and tags

- First version with `sauron -validate`: **v2.0.0-beta.3**. Older versions do
  not have it.
- Image: `ghcr.io/pokt-network/sauron:<version without v>`, for example
  `2.0.0-beta.3`. The release workflow also moves `latest` on every tag,
  prereleases included (on 2026-10-22 `latest` was `2.0.0-beta.2`), so pin
  the full version.
- Binaries: `sauron-v<version>-<os>-<arch>` on the GitHub release, for linux
  and darwin, amd64 and arm64.
- `sauron -version` prints the version it was built from.

## Ports

| Port | What | Authentication |
|---|---|---|
| 3000 | status API: `/health`, `/ready`, `/metrics`, `/{network}/status` | bearer tokens on `/{network}/status` when `auth: true` |
| 8080 | Pocket REST | none |
| 8081 | Pocket CometBFT RPC, WebSocket on `/websocket` | none |
| 8082 | Pocket gRPC | none |

The network ports are the config's `endpoints[].listen`; another network gets
its own. Keep them on a private network or behind a gateway.

## Reload and restart

Sauron watches its config file and reloads on every change, and on `SIGHUP`.

- **Live** (applied at once): internal nodes and their URLs, externals, users,
  `height_check.interval`, `archival.check_interval` and `min_height`,
  `timeouts.health_check`, circuit breakers, `advertise` URLs,
  `external_failover_threshold`.
- **Startup-only** (keep their running value until a restart): `listen`,
  `auth`, `rate_limit`, `redis`, `timeouts.proxy`, `grpc_keepalive`, the set of
  networks and their endpoints, `type`, `mode`, `height_check` other than
  `interval`, whether `archival` is set, `sync`, and `grpc_insecure` on a node
  whose gRPC URL did not change. The rest of the file is still applied,
  `sauron_config_reload_ok` drops to 0 and the log names the fields.
- **Rejected**: a file that fails validation is not applied at all; the
  running config stays and the log says why.

## Release binary

The same steps as the runbook, without Docker:

```bash
curl -sLO https://github.com/pokt-network/sauron/releases/download/v2.0.0-beta.3/sauron-v2.0.0-beta.3-linux-amd64
install -m 0755 sauron-v2.0.0-beta.3-linux-amd64 ./sauron
./sauron -version
./sauron -validate -config examples/pocket/config/config.yaml
./sauron -config examples/pocket/config/config.yaml
```

Expect `Sauron v2.0.0-beta.3`, then `...: valid: 1 networks, 1 internal nodes,
0 externals` and the stand-in's `1 internal node` warning; then run the runbook's checks from
[step 5](DOCKER_COMPOSE.md#step-5-check-the-status-api) on, with `./sauron`
where they say `$C exec sauron /app/sauron`. Not run as written:
v2.0.0-beta.3 was not published when this was written, so the download is
unverified; `-validate` and the start were run on a binary built from the same
commit. Run it under your process manager with a memory limit; `SIGTERM` stops
it.

## Kubernetes

No manifests ship in this repository. What a manifest needs:

- **Image** pinned by version, `args: ["-config", "/app/config/config.yaml"]`.
- **Config** mounted as a directory at `/app/config`, from a ConfigMap, or a
  Secret when it holds `externals[].token` or `users[].token`. No `subPath`:
  a `subPath` mount never sees updates. Sauron's watcher follows the volume's
  symlink swap (from viper's source, `WatchConfig`; not measured on a cluster
  here), so live fields apply without a restart.
- **Validate the file before applying it**: run the same image with
  `-validate` against it, locally or in CI.
- **Probes**: liveness `GET /health` on 3000. Readiness `GET /ready` returns
  200 as soon as internal nodes are configured, whether or not one is routable
  (issue #29), so it does not take a replica out when its nodes are down.
- **Replicas** need nothing shared: each checks and routes on its own.
- **Services**: one for the status port, and one per network port your clients
  use. gRPC needs HTTP/2 end to end: an L4 Service passes it; an ingress must
  speak gRPC to the pod.
