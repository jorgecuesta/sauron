# AGENTS.md

Read this first. It says what Sauron is, routes you to the right document, and
lists what breaks a deployment.

## Hard rules

1. **Run `sauron -validate` after every config edit** and get exit 0 before you
   start or restart anything. Read its `warning:` lines too: a warning is a
   config that loads but routes nothing on some port, or a network with
   nothing to fail over to. With Docker Compose:
   `docker compose -f examples/pocket/docker-compose.yaml run --rm sauron -validate -config /app/config/config.yaml`.
2. **Never invent a node URL, a port or a network name.** Node addresses come
   from the human. Check that each one answers before you configure it
   ([runbook step 8](docs/deploy/DOCKER_COMPOSE.md#step-8-put-your-own-nodes-in)).
3. **The proxy ports have no authentication.** Never publish them to the
   internet or open a firewall for them without the human's explicit OK. Only
   the status API has bearer tokens (`auth`), and only for `/{network}/status`.
4. **Stop and ask before touching a Sauron that serves real traffic.** That
   includes a production config, a deployed container and a Kubernetes
   manifest someone else runs.
5. **Check the ports before starting** (runbook step 0). If one is taken, use
   the runbook's alternate set (33000, 38080, 38081, 38082) when it is free,
   and tell the human; never stop or kill the program that holds a port.
6. **Image tags have no leading `v`; release binaries do.** Image
   `ghcr.io/pokt-network/sauron:2.0.0-beta.3`, binary
   `sauron-v2.0.0-beta.3-linux-amd64`. Never use `latest`: every release tag,
   betas included, moves it.
7. **Some changes need a restart.** After a reload, `sauron_config_reload_ok`
   at 0 means startup-only fields changed and keep their old values until a
   restart (list in [the deploy README](docs/deploy/README.md#reload-and-restart)).
8. **`/ready` at 200 does not mean a node is routable** (issue #29). Prove
   routing with `sauron_node_available` and a real request through each port.
9. **The example's node is a stand-in.** It is the Pocket Network
   Foundation's public beta endpoint. Replace it with the human's own nodes
   before real traffic goes through, and never point load at it.
10. **Follow the runbook with the example's own files.** Edit
    `examples/pocket/config/config.yaml` and `examples/pocket/.env`; do not
    write your own compose file, or the runbook's checks no longer describe
    what runs.

## What this is

Sauron is a routing proxy for blockchain nodes. Clients (RelayMiners,
Gateways, Indexers) point at Sauron instead of at one node. For each network
it serves, Sauron checks every node's block height and the health of each
protocol it exposes, and sends each request to a healthy node at the highest
height, round-robin among them. A node that falls behind, stops answering or
fails requests leaves rotation; it comes back when its checks pass.

- One process serves many networks. Each network has a `type` (`cosmos`,
  `evm`, `solana`, `custom`) whose adapter knows how to read that chain's height.
- Per network, Sauron listens on one port per protocol: for Pocket Network
  (a Cosmos chain) REST on 8080, CometBFT RPC on 8081 (WebSocket on
  `/websocket`), gRPC on 8082. The status API (`/health`, `/ready`,
  `/metrics`, `/{network}/status`) is on 3000.
- It holds no keys, signs nothing and writes nothing on chain. It is
  stateless: each replica checks and routes on its own (an optional Redis only
  caches heights).
- Several Sauron deployments can list each other as `externals` and use each
  other's nodes when their own fall behind.

## Where to go

- **Deploy it** (the usual task): [docs/deploy/README.md](docs/deploy/README.md)
  to pick a path, then [docs/deploy/DOCKER_COMPOSE.md](docs/deploy/DOCKER_COMPOSE.md),
  the runbook. Its steps 0 to 7 start Sauron on Pocket beta with nothing of the
  human's; step 8 puts their nodes in.
- **It does not start, or does not route**: [docs/deploy/TROUBLESHOOTING.md](docs/deploy/TROUBLESHOOTING.md).
- **Configure something the example does not show** (circuit breaker,
  externals, archival, EVM/Solana/custom chains): [README.md, Configuration](README.md#configuration)
  and the commented [config.default.yaml](config.default.yaml).
- **How it works inside**: [HOW_THIS_WORKS.md](HOW_THIS_WORKS.md).
- **Changing the code**: [CLAUDE.md](CLAUDE.md) has the test and quality rules.

## What the human decides

Ask, in one message, only what you cannot find out yourself:

1. Which nodes Sauron should route to: their addresses and which of REST, RPC,
   gRPC each one serves. Until they answer, run the runbook with the stand-in.
2. Beta or mainnet. Default: beta.
3. Where it runs: Docker Compose on this host (the runbook), the release
   binary, or Kubernetes ([notes](docs/deploy/README.md#kubernetes)). Default:
   Docker Compose.

## Invariants

1. **`grpc_insecure` matches the node**: `true` for plaintext gRPC (pocketd's
   default on 9090 inside a private network), `false` for TLS. A gRPC address
   is `host:port`, never `https://...`.
2. **At least 2 internal nodes per network** for failover. With 1, a node
   that fails leaves clients with `503 No available nodes`.
3. **Node addresses are reached from where Sauron runs.** Inside a container,
   `localhost` is the container: a node on the same host needs the host's IP.
4. **The config is one YAML file.** In Docker Compose and Kubernetes mount its
   directory, not the file (a file mount keeps the old copy when an editor
   replaces the file; a Kubernetes `subPath` mount never updates).
5. **One version per deployment**, pinned by tag.
