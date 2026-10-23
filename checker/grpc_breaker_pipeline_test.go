package checker

import (
	"context"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"sauron/config"
	"sauron/internal/testutil"
	"sauron/proxy"
	"sauron/selector"
	"sauron/storage"
)

// TestGRPCBrokenNodeStaysOutUntilItAnswers (#10): the proxy's circuit breaker
// takes a node failing every query out of rotation, and the gRPC health check
// keeps it out while it is still broken, so clients never hit it again; once
// it answers, the next check puts it back. Wired as server.New does: one
// HealthStore shared by the selector, the circuit breaker and the scheduler.
func TestGRPCBrokenNodeStaysOutUntilItAnswers(t *testing.T) {
	t.Parallel()
	const network = "testnet"

	good := newGRPCAnswerBackend(t, codes.OK, false)
	broken := newGRPCAnswerBackend(t, codes.Unavailable, false)

	yaml := `
listen: ":3000"
timeouts:
  health_check: 3s
  proxy: 5s
networks:
  - name: ` + network + `
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rpc
        listen: ":18081"
      - protocol: grpc
        listen: ":18082"
    circuit_breaker:
      http_codes: [502, 503]
      threshold: 3
      retry_attempts: 0
internals:
  - name: node-good
    network: ` + network + `
    grpc_insecure: true
    endpoints:
      rpc: "http://127.0.0.1:18099"
      grpc: "` + good.addr + `"
  - name: node-broken
    network: ` + network + `
    grpc_insecure: true
    endpoints:
      rpc: "http://127.0.0.1:18098"
      grpc: "` + broken.addr + `"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)
	heightStore := storage.NewHeightStore()
	healthStore := storage.NewHealthStore()
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
	sel.SetHealthStore(healthStore)
	cb := proxy.NewCircuitBreaker(healthStore, zap.NewNop())
	grpcProxy := proxy.NewGRPCProxy(sel, cfgLoader, endpointStore, zap.NewNop(), network, cb)
	t.Cleanup(func() { _ = grpcProxy.Close() })

	sched := &MultiChainScheduler{healthStore: healthStore, configLoader: cfgLoader, logger: zap.NewNop()}
	sched.timeout.Store(int64(2 * time.Second))
	check := func(name, addr string) {
		sched.pool = newTestPool()
		sched.scheduleGRPCHealthCheck(config.MultiChainNode{Name: name, Network: network, GRPCInsecure: true}, addr, "grpc")
		drainTestPool(sched.pool)
	}

	// Before the first check the broken node looks fine at the transport
	// level; seed it healthy as a READY-only check would have.
	for _, name := range []string{"node-good", "node-broken"} {
		heightStore.Update(network, name, "rpc", 100, time.Millisecond, "internal")
		healthStore.SetHealthy(network, name, "grpc")
	}

	proxyLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	proxySrv := grpcProxy.GetServer()
	go func() { _ = proxySrv.Serve(proxyLis) }()
	t.Cleanup(proxySrv.Stop)
	conn, err := grpc.NewClient("passthrough:///"+proxyLis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	call := func() codes.Code {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return status.Code(conn.Invoke(ctx, "/test.Service/Query", &emptypb.Empty{}, &emptypb.Empty{}))
	}

	// The breaker takes the broken node out after 3 failed client calls.
	failed := 0
	for i := 0; i < 6; i++ {
		if call() != codes.OK {
			failed++
		}
	}
	if failed != 3 || healthStore.IsHealthy(network, "node-broken", "grpc") {
		t.Fatalf("after 6 calls: %d failed, node-broken healthy=%v; want 3 and excluded", failed, healthStore.IsHealthy(network, "node-broken", "grpc"))
	}

	// Health-check cycles while it is still broken: it stays out, and no
	// client call fails.
	for cycle := 0; cycle < 3; cycle++ {
		check("node-good", good.addr)
		check("node-broken", broken.addr)
		for i := 0; i < 4; i++ {
			if code := call(); code != codes.OK {
				t.Fatalf("cycle %d call %d: %s, a client hit the still-broken node", cycle, i, code)
			}
		}
	}

	// It is fixed: the next check re-includes it and it serves again.
	broken.set(codes.OK)
	before := len(broken.calls())
	check("node-broken", broken.addr)
	if !healthStore.IsHealthy(network, "node-broken", "grpc") {
		t.Fatal("fixed node not re-included by the health check")
	}
	for i := 0; i < 4; i++ {
		if code := call(); code != codes.OK {
			t.Fatalf("call %d after recovery: %s", i, code)
		}
	}
	// 1 health query + 2 of the 4 round-robin client calls.
	if got := len(broken.calls()) - before; got != 3 {
		t.Errorf("recovered node received %d calls, want 3 (1 health query + 2 client calls)", got)
	}
}
