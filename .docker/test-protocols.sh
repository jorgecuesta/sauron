#!/bin/bash
# Deterministic L3 suite: Sauron in front of two fake Cosmos nodes
# (docker-compose.fake.yaml). Proves every client protocol end to end
# (REST, RPC GET and JSON-RPC POST, WebSocket NewBlock subscription, gRPC,
# status port) and node failover/recovery through the proxy.
#
# Every check is a strict pass/fail: there are no fallback passes. The script
# exits non-zero if any check fails, so CI can gate on it.

set -u

COMPOSE="${COMPOSE:-docker compose} -f docker-compose.fake.yaml"
STATUS_PORT="${SAURON_FAKE_STATUS_PORT:-33000}"
REST_PORT="${SAURON_FAKE_REST_PORT:-38080}"
RPC_PORT="${SAURON_FAKE_RPC_PORT:-38081}"
GRPC_PORT="${SAURON_FAKE_GRPC_PORT:-38082}"
ADMIN_A="${SAURON_FAKE_NODE_A_ADMIN_PORT:-38100}"
ADMIN_B="${SAURON_FAKE_NODE_B_ADMIN_PORT:-38101}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

RUN=0
FAILED=0
pass() { RUN=$((RUN + 1)); echo "PASS  $1"; }
fail() { RUN=$((RUN + 1)); FAILED=$((FAILED + 1)); echo "FAIL  $1"; }

cleanup() {
    if [ "$FAILED" -gt 0 ]; then
        echo "--- sauron logs (tail) ---"
        docker logs sauron-fake-sauron 2>&1 | tail -40
    fi
    $COMPOSE down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

# metric_sum <name> [label-regex]: sum of a Sauron metric's samples (sample lines only).
metric_sum() {
    curl -s "http://localhost:${STATUS_PORT}/metrics" | grep -E "^$1(\{| )" | grep -E "${2:-.}" | awk '{s+=$NF} END {print s+0}'
}

# metric_min / metric_max: lowest / highest value among the matching series
# ("none" when no series matches). sauron_node_available has one series per
# protocol, so min 1 means available on every protocol, max 0 on none.
metric_min() {
    curl -s "http://localhost:${STATUS_PORT}/metrics" | grep -E "^$1(\{| )" | grep -E "${2:-.}" | awk 'NR==1||$NF<m{m=$NF} END {print (NR?m:"none")}'
}
metric_max() {
    curl -s "http://localhost:${STATUS_PORT}/metrics" | grep -E "^$1(\{| )" | grep -E "${2:-.}" | awk 'NR==1||$NF>m{m=$NF} END {print (NR?m:"none")}'
}

# rest_node: which fake node served a REST request through Sauron ("" on non-200).
rest_node() {
    curl -s -o /dev/null -D - --max-time 5 "http://localhost:${REST_PORT}/cosmos/base/tendermint/v1beta1/blocks/latest" |
        tr -d '\r' | awk '{l=tolower($0)} l ~ /^http\//{code=$2} l ~ /^x-fake-node:/{n=$2} END{if (code==200) print n}'
}

smoke() {
    (cd "$REPO_ROOT" && go run ./tools/smoke \
        -rest "http://localhost:${REST_PORT}" \
        -rpc "http://localhost:${RPC_PORT}" \
        -grpc "localhost:${GRPC_PORT}" \
        -status "http://localhost:${STATUS_PORT}" -network pocket \
        -ws-timeout 30s)
}

echo "== Starting fake-backend environment"
$COMPOSE down -v >/dev/null 2>&1 || true
if ! $COMPOSE up -d --build --wait >/dev/null 2>&1; then
    fail "environment did not start"
    $COMPOSE ps
    exit 1
fi
pass "environment started (sauron + node-a + node-b healthy)"

echo "== Waiting for the first health-check cycle (cron every 30s)"
ELAPSED=0
until curl -sf "http://localhost:${STATUS_PORT}/pocket/status" | grep -q '"height"' && [ -n "$(rest_node)" ]; do
    [ $ELAPSED -ge 75 ] && break
    sleep 3
    ELAPSED=$((ELAPSED + 3))
done
if [ -n "$(rest_node)" ]; then pass "nodes checked and routable after ${ELAPSED}s"; else fail "no node routable after ${ELAPSED}s"; fi

echo "== Every client path through Sauron"
if smoke; then pass "smoke: REST, RPC GET/POST, WebSocket NewBlock, gRPC, status port"; else fail "smoke reported failures"; fi

echo "== gRPC response metadata reaches the client"
GRPC_OUT=$(cd "$REPO_ROOT" && go run ./tools/smoke -grpc "localhost:${GRPC_PORT}" 2>&1)
if echo "$GRPC_OUT" | grep -q "x-cosmos-block-height [0-9]"; then pass "gRPC x-cosmos-block-height forwarded"; else fail "gRPC x-cosmos-block-height missing: $GRPC_OUT"; fi

echo "== Failover: fail the node currently serving, traffic must move"
SERVING=$(rest_node)
case "$SERVING" in
    node-a) FAIL_ADMIN=$ADMIN_A; OTHER=node-b ;;
    node-b) FAIL_ADMIN=$ADMIN_B; OTHER=node-a ;;
    *) fail "could not identify the serving node (got '$SERVING')"; exit 1 ;;
esac
echo "   serving: $SERVING -> failing it"
curl -sf -X POST "http://localhost:${FAIL_ADMIN}/admin/fail" >/dev/null || fail "admin fail request"

ELAPSED=0
until [ "$(metric_max sauron_node_available "node=\"$SERVING\"")" = "0" ]; do
    [ $ELAPSED -ge 75 ] && break
    sleep 3
    ELAPSED=$((ELAPSED + 3))
done
if [ "$(metric_max sauron_node_available "node=\"$SERVING\"")" = "0" ]; then
    pass "$SERVING marked unavailable after ${ELAPSED}s"
else
    fail "$SERVING still marked available after ${ELAPSED}s"
fi

BAD=0
for i in $(seq 1 20); do
    n=$(rest_node)
    [ "$n" != "$OTHER" ] && BAD=$((BAD + 1))
done
if [ "$BAD" -eq 0 ]; then pass "20/20 REST requests served by $OTHER after failover"; else fail "$BAD/20 REST requests not served by $OTHER"; fi
if smoke >/dev/null 2>&1; then pass "all client paths still work with $SERVING down"; else fail "client paths broken with $SERVING down"; fi

echo "== Recovery: the node comes back into rotation"
curl -sf -X POST "http://localhost:${FAIL_ADMIN}/admin/recover" >/dev/null || fail "admin recover request"
ELAPSED=0
until [ "$(metric_min sauron_node_available "node=\"$SERVING\"")" = "1" ]; do
    [ $ELAPSED -ge 75 ] && break
    sleep 3
    ELAPSED=$((ELAPSED + 3))
done
if [ "$(metric_min sauron_node_available "node=\"$SERVING\"")" = "1" ]; then
    pass "$SERVING marked available again after ${ELAPSED}s"
else
    fail "$SERVING not marked available after ${ELAPSED}s"
fi

echo "== Both nodes down: Sauron must fail clearly, not hang"
curl -sf -X POST "http://localhost:${ADMIN_A}/admin/fail" >/dev/null
curl -sf -X POST "http://localhost:${ADMIN_B}/admin/fail" >/dev/null
CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "http://localhost:${REST_PORT}/cosmos/base/tendermint/v1beta1/blocks/latest")
if [ "$CODE" -ge 500 ] 2>/dev/null; then pass "REST answers $CODE (no hang) with every node down"; else fail "REST answered '$CODE' with every node down"; fi
curl -sf -X POST "http://localhost:${ADMIN_A}/admin/recover" >/dev/null
curl -sf -X POST "http://localhost:${ADMIN_B}/admin/recover" >/dev/null

echo
echo "$RUN checks, $FAILED failed"
[ "$FAILED" -eq 0 ]
