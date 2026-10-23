# Deploy with Docker Compose

This runbook starts the example in [examples/pocket/](../../examples/pocket/):
one Sauron container routing Pocket Network **beta** (chain id
`pocket-lego-testnet`) on REST, RPC (with WebSocket) and gRPC. As shipped, its
one node is the public beta endpoint (a stand-in); step 8 puts the human's
own nodes in.

It goes in two halves:

1. **Steps 0 to 7**: start it with the stand-in. Nothing of the human's is
   needed. It proves the container, the config and every client path.
2. **Steps 8 to 10**: the human's nodes, later changes, stopping.

Every **Expect** below comes from a real run on 2026-10-22, with the image
built from this repository at commit `77a9b5d` (v2.0.0-beta.3 was not yet
published), unless it says **not verified**. Heights, timestamps and ids
differ on every run; `...` marks lines left out.

How to read a step: **Run** the command, compare with **Expect**, use **If
not** when it differs, and **Stop if** says when to ask the human instead of
going on. Error messages are explained in
[TROUBLESHOOTING.md](TROUBLESHOOTING.md).

## Step 0: prerequisites

**Run**

```bash
docker compose version
```

**Expect**: `Docker Compose version v2.` or later (`v5.5.1` was used).

**If not**: `docker: 'compose' is not a docker command` means the Compose v2
plugin is missing.

**Stop if**: Docker is not installed. Do not install it yourself; ask the
human.

The container publishes ports 3000, 8080, 8081 and 8082 on this host.

**Run**

```bash
ss -ltn | grep -E ':(3000|8080|8081|8082) ' ; echo "EXIT=$?"
```

**Expect**: no port lines and `EXIT=1`.

**If not**: a line is printed. Another program holds that port, and every
check below would read it instead of Sauron (Docker does not always refuse:
see [TROUBLESHOOTING.md, "Ports"](TROUBLESHOOTING.md#ports)). Never stop or
kill that program. Use the alternate set instead. Check it is free:

```bash
ss -ltn | grep -E ':(33000|38080|38081|38082) ' ; echo "EXIT=$?"
```

On `EXIT=1`, write it to `examples/pocket/.env` (Compose reads that file by
itself; it is not in git):

```bash
cat > examples/pocket/.env <<'ENV'
SAURON_STATUS_PORT=33000
SAURON_REST_PORT=38080
SAURON_RPC_PORT=38081
SAURON_GRPC_PORT=38082
ENV
```

From here on, use 33000, 38080, 38081 and 38082 wherever this runbook says
`localhost:3000`, `localhost:8080`, `localhost:8081` and `localhost:8082` in a
command run on this host, and tell the human which ports Sauron uses. Inside
the container the ports do not change: a command that starts with
`$C exec sauron` keeps `localhost:8082`.

**Stop if**: the alternate set is taken too. Ask the human for 4 free ports.

Use `ss`, not `netstat`: `netstat` is missing on many hosts and prints
nothing, which reads as "all free".

The stand-in needs outbound HTTPS.

**Run**

```bash
curl -s -o /dev/null -w '%{http_code}\n' https://sauron-rpc.beta.infra.pocket.network/abci_info
```

**Expect**: `200`.

**Stop if**: anything else. Without it the stand-in cannot be checked; ask
the human for outbound HTTPS, or go straight to step 8 with their nodes.

## Step 1: get the repository

**Run**

```bash
git clone https://github.com/pokt-network/sauron
cd sauron
git checkout v2.0.0-beta.3
```

**Expect**: `HEAD is now at ...`.

**If not**: `error: pathspec 'v2.0.0-beta.3' did not match` means that
release is not published yet. Stay on `main`; step 3 then builds the image
from the checkout.

**Run**

```bash
ls AGENTS.md examples/pocket/docker-compose.yaml examples/pocket/config/config.yaml
```

**Expect**: the 3 paths, no `No such file or directory`.

**Stop if**: one is missing. This checkout predates this runbook.

Run every later command from the repository root. Set this once per shell:

```bash
export C="docker compose -f examples/pocket/docker-compose.yaml"
```

## Step 2: check the compose file

**Run**

```bash
$C config --quiet ; echo "EXIT=$?"
```

**Expect**: `EXIT=0`.

**If not**: the error names the line of `docker-compose.yaml` or `.env` that
is wrong. Undo your edit to it.

## Step 3: validate the config

First, which image will run. An image built from a checkout keeps the
release's tag, so an older build would run instead of this checkout's code.

**Run**

```bash
$C run --rm sauron -version
```

**Expect**: `Sauron v2.0.0-beta.3`, the released image (pulled now if it
was not on this host). Or, when the tag cannot be pulled, Compose builds it
from the checkout first and it prints `Sauron built-from-checkout`.

**If not**: `Sauron built-from-checkout` without a `Building` line just
before it: an image built earlier, maybe from another checkout or commit,
holds the tag. If v2.0.0-beta.3 is published (step 1's checkout worked),
replace it with the release; otherwise rebuild it from this checkout. Then run
`-version` again.

```bash
$C pull     # the release is published
$C build    # it is not
```

**Run**

```bash
$C run --rm sauron -validate -config /app/config/config.yaml
```

**Expect**: Compose lines (`Network sauron-pocket_default Creating`,
`Container sauron-pocket-sauron-run-<id> Creating`, `Created`: the
throwaway container of `run`), then

```
/app/config/config.yaml: valid: 1 networks, 1 internal nodes, 0 externals
/app/config/config.yaml: warning: network "pocket": 1 internal node; if it fails, clients get no available nodes (list at least 2 to fail over)
```

The warning is expected with the stand-in, which is 1 node; step 8 lists at
least 2 of the human's.

A missing warning line means the image is not this checkout's: go back to
the start of this step and `$C build`.

The first `run` on a host pulls the image, or builds it when the tag cannot
be pulled (not published yet, or no access to ghcr.io):
`Image ghcr.io/pokt-network/sauron:2.0.0-beta.3 Pulling`, then `Building`
and the Docker build steps, a few minutes.

**If not**: the line says what is wrong and in which file; see
[TROUBLESHOOTING.md, "Validation errors"](TROUBLESHOOTING.md#validation-errors).

**Stop if**: you edited nothing and it still fails: the checkout is not what
this runbook describes.

## Step 4: start it

**Run**

```bash
$C up -d
sleep 10
$C ps --format '{{.Name}} {{.Status}}'
```

**Expect**:

```
 Container sauron-pocket-sauron-1 Started
sauron-pocket-sauron-1 Up 13 seconds (healthy)
```

**If not**: `(health: starting)`: wait 10 seconds and look again.
`Restarting` or `Exited`: read `$C logs sauron`; the last line says why.

## Step 5: check the status API

**Run**

```bash
$C exec sauron /app/sauron -version
curl -s -w ' %{http_code}\n' localhost:3000/health
curl -s -w ' %{http_code}\n' localhost:3000/ready
curl -s localhost:3000/pocket/status ; echo
```

**Expect**:

```
Sauron v2.0.0-beta.3
OK 200
Ready 200
{"height":756321}
```

A build from the checkout prints `Sauron built-from-checkout` (measured)
instead of the version; the release image prints the tag (not verified until
v2.0.0-beta.3 is published). `/pocket/status` lists `endpoints` too once `advertise` URLs are set.

**If not**: `connection refused`: the container is not up (step 4) or the
port is mapped elsewhere (`.env`). `{"height":0}` in the first 30 seconds:
the first check has not finished; wait and repeat.

`/ready` at 200 does not prove a node is routable (issue #29). Step 6 does.

## Step 6: check the node

**Run**

```bash
curl -s localhost:3000/metrics | grep -E '^sauron_node_(height|available|websocket_available)\{'
```

**Expect**:

```
sauron_node_available{network="pocket",node="pnf-beta-public",type="rpc"} 1
sauron_node_height{network="pocket",node="pnf-beta-public",source="internal",type="rpc"} 756321
sauron_node_websocket_available{network="pocket",node="pnf-beta-public",type="websocket"} 0
```

`sauron_node_available` 1 and a height that grows on the next run (beta made
about 2 blocks a minute on 2026-10-22) mean the node is checked and routable.

`sauron_node_websocket_available` was 0 on beta: the public beta endpoint
answered every WebSocket upgrade with `503 WebSocket not supported by selected
backend` on 2026-10-22, while the mainnet endpoint answered `101`. That is the
stand-in, not your deployment; with your own nodes it is 1 when their RPC
serves `/websocket`.

**If not**: `sauron_node_available ... 0`: the node failed its height check;
`$C logs sauron | grep 'Height check failed'` gives the reason.

## Step 7: send a request through each port

**Run**

```bash
curl -s localhost:8080/cosmos/base/tendermint/v1beta1/blocks/latest | jq -c '{chain_id: .block.header.chain_id, height: .block.header.height}'
curl -s localhost:8081/abci_info | jq -c '.result.response | {data, version, last_block_height}'
$C exec sauron grpcurl -plaintext localhost:8082 cosmos.base.tendermint.v1beta1.Service/GetLatestBlock | grep -m2 -E '"(chainId|height)"'
```

**Expect**:

```
{"chain_id":"pocket-lego-testnet","height":"756322"}
{"data":"pocket","version":"0.1.36","last_block_height":"756322"}
      "chainId": "pocket-lego-testnet",
      "height": "756349",
```

`grpcurl` runs inside the container (the image ships it), so nothing is
installed on this host. Without `jq`, drop the `| jq ...` part and look for
the same fields in the JSON.

The WebSocket path, as a handshake:

**Run**

```bash
curl -s -i -m 5 -N --http1.1 -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
  localhost:8081/websocket | head -1
```

**Expect**, with the beta stand-in: `HTTP/1.1 503 Service Unavailable`, body
`No available nodes`, because no node passes the WebSocket check (step 6).
With nodes whose WebSocket works: `HTTP/1.1 101 Switching Protocols` (seen
through Sauron with the mainnet endpoint, which then delivered a `NewBlock`
event).

**If not**: `No available nodes` on REST, RPC or gRPC: no node is healthy for
that protocol; see [TROUBLESHOOTING.md](TROUBLESHOOTING.md#no-available-nodes).

Steps 0 to 7 done: Sauron runs and routes, **to the stand-in only**. It is
not yet something a RelayMiner or any real client should use: that needs
step 8. Once step 8 is done, clients point at `http://<this host>:8080`
(REST), `http://<this host>:8081` (RPC and WebSocket) and `<this host>:8082`
(gRPC, plaintext).

## Step 8: put your own nodes in

**Stop and ask the human** for their nodes, in one message: the address of
each one, and which of REST (1317), RPC (26657) and gRPC (9090) it serves,
plaintext or TLS. Never guess an address or a port.

Check each node answers **from where Sauron runs**. Inside the container,
`localhost` is the container itself: a node on this same host needs the
host's IP address. For each node, with its addresses:

**Run**

```bash
$C exec sauron wget -qO- http://NODE:26657/abci_info
$C exec sauron wget -qO- http://NODE:1317/cosmos/base/tendermint/v1beta1/blocks/latest | head -c 200 ; echo
$C exec sauron grpcurl -plaintext NODE:9090 cosmos.base.tendermint.v1beta1.Service/GetLatestBlock | grep -m1 '"height"'
```

(`grpcurl` without `-plaintext` for a TLS node.) And whether the node's
WebSocket works, which decides `sauron_node_websocket_available`:

```bash
$C exec sauron curl -s -i -m 5 -N --http1.1 -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
  http://NODE:26657/websocket | head -1
```

`HTTP/1.1 101 Switching Protocols` means it does (measured against the
mainnet public endpoint); anything else means WebSocket clients will not be
routed to that node.

**Expect**: a JSON with `"data":"pocket"` and a `last_block_height`, a JSON
starting `{"block_id":`, and a `"height"` line:

```
{"jsonrpc":"2.0","id":-1,"result":{"response":{"data":"pocket","version":"0.1.36","last_block_height":"756356",...}}}
{"block_id":{"hash":"tBXBlnWAUH1lkkrj3x48OrK6JMiauYk27NHKfjYiDeQ=",...
      "height": "756356",
```

Measured with the stand-in's addresses in place of `NODE` (`https://` URLs,
and gRPC over TLS without `-plaintext`), not with a human's node.

**Stop if**: a node does not answer. Tell the human which node and which
port; do not configure it.

Then edit `examples/pocket/config/config.yaml`: replace the
`pnf-beta-public` entry with one entry per node, as in the commented
"Your own nodes" block at the end of the file. List at least 2 nodes so Sauron
can fail over. `grpc_insecure: true` for plaintext gRPC, `false` for TLS; the
gRPC address is `host:port`, never `https://`.

**Run**

```bash
$C run --rm sauron -validate -config /app/config/config.yaml
```

**Expect**: `valid: 1 networks, N internal nodes, 0 externals` with your N
(2 or more), and no `warning:` line.

**If not**: fix what the line names and run it again. A `warning: ... has no
internal node with a "rest" URL` means no node has that protocol's URL
(often a misspelled key); clients on that port get `No available nodes`.

The running container picks the file up by itself: nodes are live. Within one
check interval (30 seconds):

**Run**

```bash
curl -s localhost:3000/metrics | grep -E '^sauron_node_(available|height)\{'
curl -s localhost:3000/metrics | grep '^sauron_config_reload_ok'
```

**Expect** one `sauron_node_available ... 1` and one height per node, and
`sauron_config_reload_ok 1`. Measured by adding a second entry (`node-2`):

```
sauron_node_available{network="pocket",node="node-2",type="rpc"} 1
sauron_node_available{network="pocket",node="pnf-beta-public",type="rpc"} 1
sauron_node_height{network="pocket",node="node-2",source="internal",type="rpc"} 756349
sauron_node_height{network="pocket",node="pnf-beta-public",source="internal",type="rpc"} 756349
sauron_config_reload_ok 1
```

Then run step 7 again.

## Step 9: change the config later

Edit the file, run the `-validate` command of step 8, and check
`sauron_config_reload_ok`.

- `1`: everything applied. The log shows `Configuration reloaded
  successfully`.
- `0`: a startup-only field changed. The log says which, for example
  `"error":"configuration change requires a restart: networks[pocket].height_check changed"`.
  The rest of the file was applied. Restart to apply it:

  **Run**

  ```bash
  $C restart
  sleep 8
  curl -s localhost:3000/metrics | grep '^sauron_config_reload_ok'
  ```

  **Expect**: ` Container sauron-pocket-sauron-1 Started`, then
  `sauron_config_reload_ok 1`.

`$C kill -s SIGHUP sauron` also reloads; Compose prints `Container ...
Killed`, but the signal only reloads and the container keeps running. The
list of live and startup-only fields is in
[README.md, "Reload and restart"](README.md#reload-and-restart).

## Step 10: stop it

**Run**

```bash
$C down
```

**Expect**: ` Container sauron-pocket-sauron-1 Removed` and ` Network
sauron-pocket_default Removed`. Nothing is kept: Sauron has no state on disk.

## Switching to mainnet

The same runbook with mainnet nodes. For the stand-in, replace `.beta.infra`
with `.infra` in the three URLs of `examples/pocket/config/config.yaml`
(`sauron-api.infra.pocket.network`, `sauron-rpc.infra.pocket.network`,
`sauron-grpc.infra.pocket.network:443`); the chain id in step 7 is then
`pocket`. Sauron only reads from nodes, but **stop and ask** before routing
anyone's real traffic: use the human's own mainnet nodes for that.
