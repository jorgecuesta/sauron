package proxy

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"sauron/internal/testutil"
	"sauron/selector"
	"sauron/storage"
)

// Backend behaviours for grpcBreakerHarness nodes.
const (
	modeOK                int32 = iota // answers with its node name
	modeUnavailable                    // every call fails with Unavailable (backend origin)
	modeNotFound                       // every call fails with NotFound: the node works, the query is bad
	modeResourceExhausted              // every call fails with ResourceExhausted (rate limited)
)

// grpcBreakerHarness wires two internal gRPC nodes (node-a, node-b) at the same
// height behind a GRPCProxy, as server.New does: shared HealthStore for the
// selector and the circuit breaker, network circuit_breaker config.
type grpcBreakerHarness struct {
	conn        *grpc.ClientConn
	healthStore *storage.HealthStore
	cb          *CircuitBreaker
	modes       map[string]*atomic.Int32
}

// newGRPCBreakerHarness builds the harness. withCB passes a circuit breaker to
// the proxy; withCfg adds the network's circuit_breaker section (threshold 3),
// with grpcCodes as its grpc_codes when given.
func newGRPCBreakerHarness(t *testing.T, network string, withCB, withCfg bool, grpcCodes ...string) *grpcBreakerHarness {
	t.Helper()
	h := &grpcBreakerHarness{modes: map[string]*atomic.Int32{}}
	addrs := map[string]string{}
	for _, name := range []string{"node-a", "node-b"} {
		mode := &atomic.Int32{}
		h.modes[name] = mode
		handler := func(_ interface{}, stream grpc.ServerStream) error {
			var f rawFrame
			if err := stream.RecvMsg(&f); err != nil {
				return err
			}
			switch mode.Load() {
			case modeUnavailable:
				return status.Error(codes.Unavailable, "node is broken")
			case modeNotFound:
				return status.Error(codes.NotFound, "no such block")
			case modeResourceExhausted:
				return status.Error(codes.ResourceExhausted, "slow down")
			}
			return stream.SendMsg(&rawFrame{payload: []byte(name)})
		}
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("backend listen: %v", err)
		}
		srv := grpc.NewServer(grpc.UnknownServiceHandler(handler), grpc.ForceServerCodec(&rawCodec{}))
		go func() { _ = srv.Serve(lis) }()
		t.Cleanup(srv.Stop)
		addrs[name] = lis.Addr().String()
	}

	cbSection := ""
	if withCfg {
		cbSection = `
    circuit_breaker:
      http_codes: [502, 503]
      threshold: 3
      retry_attempts: 0`
		if len(grpcCodes) > 0 {
			cbSection += "\n      grpc_codes: [" + strings.Join(grpcCodes, ", ") + "]"
		}
	}
	yaml := `
listen: ":3000"
timeouts:
  health_check: 5s
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
        listen: ":18082"` + cbSection + `
internals:
  - name: node-a
    network: ` + network + `
    grpc_insecure: true
    endpoints:
      rpc: "http://127.0.0.1:18099"
      grpc: "` + addrs["node-a"] + `"
  - name: node-b
    network: ` + network + `
    grpc_insecure: true
    endpoints:
      rpc: "http://127.0.0.1:18098"
      grpc: "` + addrs["node-b"] + `"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)
	heightStore := storage.NewHeightStore()
	h.healthStore = storage.NewHealthStore()
	for _, name := range []string{"node-a", "node-b"} {
		heightStore.Update(network, name, "rpc", 100, time.Millisecond, "internal")
		h.healthStore.SetHealthy(network, name, "grpc")
	}
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
	sel.SetHealthStore(h.healthStore)
	if withCB {
		h.cb = NewCircuitBreaker(h.healthStore, zap.NewNop())
	}
	grpcProxy := NewGRPCProxy(sel, cfgLoader, endpointStore, zap.NewNop(), network, h.cb)
	t.Cleanup(func() { _ = grpcProxy.Close() })

	proxyLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	proxySrv := grpcProxy.GetServer()
	go func() { _ = proxySrv.Serve(proxyLis) }()
	t.Cleanup(proxySrv.Stop)

	h.conn, err = grpc.NewClient("passthrough:///"+proxyLis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(&rawCodec{})),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = h.conn.Close() })
	return h
}

// call makes one request-response call and returns the serving node's name
// (empty on error) and the status code.
func (h *grpcBreakerHarness) call(t *testing.T) (string, codes.Code) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := h.conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, "/test.Service/Query")
	if err != nil {
		return "", status.Code(err)
	}
	if err := stream.SendMsg(&rawFrame{payload: []byte("q")}); err != nil {
		return "", status.Code(err)
	}
	_ = stream.CloseSend()
	var resp rawFrame
	if err := stream.RecvMsg(&resp); err != nil {
		return "", status.Code(err)
	}
	return string(resp.payload), codes.OK
}

// TestGRPCProxy_FailingNodeExcludedAndReincluded (#10): a node failing every
// call with a backend Unavailable trips the breaker and leaves rotation; once
// the health check marks it healthy again and it answers, it is back in
// rotation with its error count reset.
func TestGRPCProxy_FailingNodeExcludedAndReincluded(t *testing.T) {
	t.Parallel()
	const network = "grpc-cb-net"
	h := newGRPCBreakerHarness(t, network, true, true)
	h.modes["node-b"].Store(modeUnavailable)

	// Round-robin alternates; 6 calls give node-b its 3 failures.
	failures := 0
	for i := 0; i < 6; i++ {
		if _, code := h.call(t); code == codes.Unavailable {
			failures++
		}
	}
	if failures != 3 {
		t.Fatalf("failures before the trip = %d, want 3", failures)
	}
	if h.healthStore.IsHealthy(network, "node-b", "grpc") {
		t.Fatalf("node-b still healthy after 3 backend Unavailable (error count %d)", h.cb.GetErrorCount(network, "node-b", "grpc"))
	}

	// Excluded: every call is served by node-a.
	for i := 0; i < 6; i++ {
		if node, code := h.call(t); code != codes.OK || node != "node-a" {
			t.Fatalf("call %d after the trip: node %q code %s, want node-a OK", i, node, code)
		}
	}
	if !h.healthStore.IsHealthy(network, "node-a", "grpc") {
		t.Error("node-a marked unhealthy by node-b's failures")
	}

	// Recovery: the node is fixed and the scheduler's health check (READY ->
	// SetHealthy, checker.scheduleGRPCHealthCheck) marks it healthy again.
	h.modes["node-b"].Store(modeOK)
	h.healthStore.SetHealthy(network, "node-b", "grpc")
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		node, code := h.call(t)
		if code != codes.OK {
			t.Fatalf("call %d after recovery: code %s", i, code)
		}
		seen[node]++
	}
	if seen["node-a"] != 3 || seen["node-b"] != 3 {
		t.Errorf("distribution after recovery = %v, want 3/3", seen)
	}
	if got := h.cb.GetErrorCount(network, "node-b", "grpc"); got != 0 {
		t.Errorf("node-b error count after successful calls = %d, want 0", got)
	}
}

// A backend status that is not a node failure (NotFound) proves the node
// answers: it resets the error count instead of adding to it.
func TestGRPCProxy_NonFailureStatusResetsBreaker(t *testing.T) {
	t.Parallel()
	const network = "grpc-cb-notfound-net"
	h := newGRPCBreakerHarness(t, network, true, true)

	// Two real failures on node-b, below the threshold of 3.
	h.modes["node-b"].Store(modeUnavailable)
	for i := 0; i < 4; i++ {
		h.call(t)
	}
	if got := h.cb.GetErrorCount(network, "node-b", "grpc"); got != 2 {
		t.Fatalf("node-b error count = %d after 2 failures, want 2", got)
	}

	// The node now answers NotFound: alive, so the count resets.
	h.modes["node-a"].Store(modeNotFound)
	h.modes["node-b"].Store(modeNotFound)
	for i := 0; i < 10; i++ {
		if _, code := h.call(t); code != codes.NotFound {
			t.Fatalf("call %d: code %s, want NotFound passed through", i, code)
		}
	}
	for _, node := range []string{"node-a", "node-b"} {
		if got := h.cb.GetErrorCount(network, node, "grpc"); got != 0 {
			t.Errorf("%s error count = %d after NotFound answers, want 0", node, got)
		}
		if !h.healthStore.IsHealthy(network, node, "grpc") {
			t.Errorf("%s excluded for answering NotFound", node)
		}
	}
}

// A client that cancels says nothing about the node.
func TestGRPCProxy_ClientCancelDoesNotFeedBreaker(t *testing.T) {
	t.Parallel()
	const network = "grpc-cb-cancel-net"
	h := newGRPCBreakerHarness(t, network, true, true)

	for i := 0; i < 6; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		stream, err := h.conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, "/test.Service/Query")
		if err == nil {
			// No SendMsg: the backend waits in RecvMsg until the cancel.
			var resp rawFrame
			_ = stream.RecvMsg(&resp)
		}
		cancel()
	}
	time.Sleep(200 * time.Millisecond) // let the proxy handlers finish
	for _, node := range []string{"node-a", "node-b"} {
		if got := h.cb.GetErrorCount(network, node, "grpc"); got != 0 {
			t.Errorf("%s error count = %d after client cancels, want 0", node, got)
		}
		if !h.healthStore.IsHealthy(network, node, "grpc") {
			t.Errorf("%s excluded after client cancels", node)
		}
	}
}

// Full matrix of the two optional pieces: the breaker only acts when the
// proxy has one AND the network configures it ("wired but unconfigured" and
// "not wired" leave the failing node in rotation, without panicking).
func TestGRPCProxy_BreakerCombinations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		network     string
		withCB      bool
		withCfg     bool
		wantExclude bool
	}{
		{name: "breaker and config", network: "grpc-cb-both", withCB: true, withCfg: true, wantExclude: true},
		{name: "breaker wired, network unconfigured", network: "grpc-cb-wired", withCB: true, withCfg: false, wantExclude: false},
		{name: "config without breaker", network: "grpc-cb-cfg", withCB: false, withCfg: true, wantExclude: false},
		{name: "neither", network: "grpc-cb-none", withCB: false, withCfg: false, wantExclude: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newGRPCBreakerHarness(t, tt.network, tt.withCB, tt.withCfg)
			h.modes["node-b"].Store(modeUnavailable)
			for j := 0; j < 10; j++ {
				h.call(t)
			}
			if excluded := !h.healthStore.IsHealthy(tt.network, "node-b", "grpc"); excluded != tt.wantExclude {
				t.Errorf("node-b excluded = %v, want %v", excluded, tt.wantExclude)
			}
			if !h.healthStore.IsHealthy(tt.network, "node-a", "grpc") {
				t.Error("healthy node-a excluded")
			}
		})
	}
}

// circuit_breaker.grpc_codes replaces the default failure set: with
// [resource_exhausted], a rate-limiting node is taken out and an Unavailable
// does not count (the node answered, as with an HTTP code not in http_codes).
func TestGRPCProxy_ConfiguredGRPCCodes(t *testing.T) {
	t.Parallel()

	t.Run("listed code trips the breaker", func(t *testing.T) {
		t.Parallel()
		const network = "grpc-cb-codes-listed"
		h := newGRPCBreakerHarness(t, network, true, true, "resource_exhausted")
		h.modes["node-b"].Store(modeResourceExhausted)
		for i := 0; i < 6; i++ {
			h.call(t)
		}
		if h.healthStore.IsHealthy(network, "node-b", "grpc") {
			t.Errorf("node-b healthy after 3 ResourceExhausted with grpc_codes [resource_exhausted] (errors %d)", h.cb.GetErrorCount(network, "node-b", "grpc"))
		}
	})
	t.Run("default code not listed does not count", func(t *testing.T) {
		t.Parallel()
		const network = "grpc-cb-codes-unlisted"
		h := newGRPCBreakerHarness(t, network, true, true, "resource_exhausted")
		h.modes["node-b"].Store(modeUnavailable)
		for i := 0; i < 6; i++ {
			h.call(t)
		}
		if !h.healthStore.IsHealthy(network, "node-b", "grpc") {
			t.Error("node-b excluded for Unavailable, which grpc_codes [resource_exhausted] does not list")
		}
		if got := h.cb.GetErrorCount(network, "node-b", "grpc"); got != 0 {
			t.Errorf("node-b error count = %d, want 0", got)
		}
	})
	t.Run("default set without grpc_codes ignores ResourceExhausted", func(t *testing.T) {
		t.Parallel()
		const network = "grpc-cb-codes-default"
		h := newGRPCBreakerHarness(t, network, true, true)
		h.modes["node-b"].Store(modeResourceExhausted)
		for i := 0; i < 6; i++ {
			h.call(t)
		}
		if !h.healthStore.IsHealthy(network, "node-b", "grpc") {
			t.Error("node-b excluded for ResourceExhausted under the default set")
		}
	})
}

// #20: when a node's gRPC URL changes or the node is removed, the proxy closes
// the pooled connection to the old address instead of leaking it; a kept
// address keeps its connection.
func TestGRPCProxy_CloseConnectionsExcept(t *testing.T) {
	t.Parallel()
	h := setupGRPCProxyTest(t, func(_ interface{}, stream grpc.ServerStream) error {
		var f rawFrame
		if err := stream.RecvMsg(&f); err != nil {
			return err
		}
		return stream.SendMsg(&f)
	})
	echo := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		stream, err := h.conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, "/test.Service/Echo")
		if err != nil {
			return err
		}
		if err := stream.SendMsg(&rawFrame{payload: []byte("x")}); err != nil {
			return err
		}
		_ = stream.CloseSend()
		var f rawFrame
		return stream.RecvMsg(&f)
	}
	if err := echo(); err != nil {
		t.Fatalf("first call: %v", err)
	}

	h.proxy.connMu.RLock()
	var addr string
	var conn *grpc.ClientConn
	for a, c := range h.proxy.connPool {
		addr, conn = a, c
	}
	h.proxy.connMu.RUnlock()
	if conn == nil {
		t.Fatal("no pooled connection after a call")
	}

	if n := h.proxy.CloseConnectionsExcept(func(a string) bool { return a == addr }); n != 0 {
		t.Errorf("closed %d connections for a kept address", n)
	}
	if conn.GetState() == connectivity.Shutdown {
		t.Fatal("kept connection was closed")
	}

	if n := h.proxy.CloseConnectionsExcept(func(string) bool { return false }); n != 1 {
		t.Errorf("closed %d connections, want 1", n)
	}
	if conn.GetState() != connectivity.Shutdown {
		t.Errorf("removed address's connection state = %s, want SHUTDOWN", conn.GetState())
	}
	h.proxy.connMu.RLock()
	left := len(h.proxy.connPool)
	h.proxy.connMu.RUnlock()
	if left != 0 {
		t.Errorf("%d connections left in the pool", left)
	}
	// A node still routed to the address gets a fresh connection.
	if err := echo(); err != nil {
		t.Errorf("call after cleanup: %v", err)
	}
	// After Close, cleanup is a no-op.
	_ = h.proxy.Close()
	if n := h.proxy.CloseConnectionsExcept(func(string) bool { return false }); n != 0 {
		t.Errorf("cleanup after Close closed %d", n)
	}
}
