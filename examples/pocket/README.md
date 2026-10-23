# Pocket Network example

One Sauron container routing Pocket Network beta on REST (8080), CometBFT RPC
and WebSocket (8081) and gRPC (8082), with the status API on 3000.

| File | What |
|---|---|
| [docker-compose.yaml](docker-compose.yaml) | the container: pinned image (built from this checkout when it cannot be pulled), the config directory mounted read-only, host ports from `.env` |
| [config/config.yaml](config/config.yaml) | the Sauron config: network `pocket`, type `cosmos`, and one stand-in node |
| `.env` (yours, not in git) | host port overrides: `SAURON_STATUS_PORT`, `SAURON_REST_PORT`, `SAURON_RPC_PORT`, `SAURON_GRPC_PORT` |

The stand-in node is the Pocket Network Foundation's public beta endpoint, so
the example starts before you have a node. Replace it with your own `pocketd`
nodes (a commented block at the end of the config shows how) before real
traffic goes through it.

Follow the runbook, [docs/deploy/DOCKER_COMPOSE.md](../../docs/deploy/DOCKER_COMPOSE.md),
from step 0. In short:

```bash
export C="docker compose -f examples/pocket/docker-compose.yaml"
$C run --rm sauron -validate -config /app/config/config.yaml
$C up -d
curl -s localhost:3000/metrics | grep -E '^sauron_node_(height|available)\{'
```
