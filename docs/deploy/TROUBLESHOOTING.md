# Troubleshooting

Each entry gives the message as Sauron prints it, what it means, and the fix.
Messages are from real runs on 2026-10-22 unless marked **not verified**.

## Validation errors

From `sauron -validate`, and at startup (the server prints the same
`invalid configuration: ...` and exits). A reload that fails them is not
applied; the running config stays.

| Message (after the file name) | Meaning | Fix |
|---|---|---|
| `invalid configuration: network 0 (pocket): type cannot be empty` | the network has no `type` | `type: cosmos` for Pocket |
| `invalid configuration: internal node 0 (pnf-beta-public): references unknown network "pokt"` | the node's `network` names no network in `networks` | use the network's `name` exactly |
| `invalid configuration: network 0 (pocket), endpoint "grpc": listen ":8081" conflicts with network "pocket"` | two endpoints listen on one port | one port per endpoint, across all networks |
| `invalid configuration: network 0 (pocket): height_check interval too short: 500ms (minimum 1s)` | an interval under 1s | `1s` or more; the default is `30s` |
| `invalid configuration: health_check timeout cannot be zero; unknown keys (misspelled, or from the V1 format): internals[0].api, networks[0].api_listen` | a V1 config (`api_listen`, `internals[].api`) | rewrite it in the V2 format of [examples/pocket/config/config.yaml](../../examples/pocket/config/config.yaml) |
| `unknown keys (misspelled, or from the V1 format): internals[0].grpc_insecur` | a key Sauron does not read; the server would ignore it and start | fix the spelling |
| `internal node "pnf-beta-public": grpc "https://sauron-grpc.beta.infra.pocket.network:443" must be host:port without a scheme (TLS is set by grpc_insecure)` | a gRPC address written as a URL; the server would start, and never route to that node's gRPC (measured: `no available nodes`) | `host:port`, with `grpc_insecure: false` for TLS |
| `failed to read config: ...` | the file is missing, unreadable or not YAML | check the path (`/app/config/config.yaml` inside the container) and the YAML |

The server tolerates unknown keys and gRPC URLs, for files that ran before
v2.0.0-beta.3, and logs them as `Unknown configuration keys ignored` and
`Configuration problem`; `-validate` fails on them.

## Warnings

`-validate` exits 0 and prints, on stderr:

```
...: warning: network "pocket": endpoint "rest" has no internal node with a "rest" URL; only external rings can serve it
```

No node of that network has a URL for that protocol, so clients on that port
get `503 No available nodes` unless an external ring serves it. Usually a
misspelled key: a node URL under `api:` instead of `rest:`, or an endpoint
`protocol: grpcs`.

```
...: warning: network "pocket": 1 internal node; if it fails, clients get no available nodes (list at least 2 to fail over)
```

The network has one node: nothing to fail over to. Expected with the
example's stand-in; add the human's nodes (runbook step 8). `no internal
node` instead of `1 internal node`: only external rings serve that network.

The server logs the same lines as `Configuration warning`.

## No available nodes

A client gets `503 Service Unavailable`, body `No available nodes` (HTTP), or
`rpc error: code = Unavailable desc = ...: no available nodes` (gRPC): no node
is healthy for that protocol right now. Find which and why:

```bash
curl -s localhost:3000/metrics | grep -E '^sauron_(node_available|routing_failures_total|height_check_errors_total)'
$C logs sauron 2>&1 | grep -E 'Height check failed|No nodes available'
```

- `sauron_node_available{...} 0` with `Height check failed ... dial tcp ...`:
  the node does not answer from where Sauron runs. Check it as in
  [runbook step 8](DOCKER_COMPOSE.md#step-8-put-your-own-nodes-in). In a
  container, `localhost` is the container. A node that answers again is back
  after its next check (measured: within the 30s interval).
- `sauron_routing_failures_total{reason="no_websocket_nodes"}` grows: a
  WebSocket upgrade found no node whose WebSocket check passes
  (`sauron_node_websocket_available` 0). The node's RPC must serve
  `/websocket`. (Sauron before v2.0.0-beta.3 also answered `503 WebSocket not
  supported by selected backend` when it picked a node without WebSocket;
  measured on beta on 2026-10-22, on every node.)
- gRPC only, while REST and RPC work: `grpc_insecure` does not match the node
  (`true` against a TLS node measured: `no available nodes`), or the address
  has a scheme (see above).
- Every node down: with one node there is nothing to fail over to. List at
  least 2.

`/ready` stays `Ready 200` through all of this: after the first round of
checks it does not follow node health, by design.

## Not ready

`GET /ready` answers `503 Service not ready: first health checks not finished`:
the first round of checks has not finished (every internal node checked
once; with only external rings, the rings queried once). Measured: about 1.5s
after start. If it lasts longer than `timeouts.health_check` plus a second,
read `$C logs sauron`: the server may not have started its checks.

## Reload did not apply

`sauron_config_reload_ok` is 0 and the log has:

```
"msg":"Configuration applied except startup-only fields; restart to apply them","trigger":"file change","error":"configuration change requires a restart: networks[pocket].height_check changed"
```

The named fields need a restart; everything else was applied. `$C restart`,
then `sauron_config_reload_ok 1`. The log line `Configuration reloaded
successfully` comes just before that warning: read both.

The file changed and nothing was logged: the container does not see the
change. Mount the config directory, not the file
([AGENTS.md, invariant 4](../../AGENTS.md#invariants)).

## Ports

Do not count on Docker to refuse a taken port. With Docker Desktop on WSL2,
`docker compose up` started the container while another process listened on
0.0.0.0:3000 (measured), and `localhost:3000` could then reach either. Native
Docker Engine on Linux refuses with `port is already allocated` (**not
verified** here). Either way, check the ports first:
[runbook step 0](DOCKER_COMPOSE.md#step-0-prerequisites).

## Version

`sauron -version` prints `Sauron built-from-checkout`: Compose built the image
from the repository because the tag could not be pulled (runbook step 3).
`flag provided but not defined: -validate` (measured on `2.0.0-beta.2`): a
version older than v2.0.0-beta.3.
