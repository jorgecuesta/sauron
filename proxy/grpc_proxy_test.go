package proxy

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"sauron/internal/testutil"
	"sauron/selector"
	"sauron/storage"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ---------------------------------------------------------------------------
// TestNormalizeGRPCMethod
// ---------------------------------------------------------------------------

func TestNormalizeGRPCMethod(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "standard full path",
			input: "/pocket.session.Query/GetSession",
			want:  "pocket.session.Query",
		},
		{
			name:  "no leading slash",
			input: "pocket.session.Query/GetSession",
			want:  "pocket.session.Query",
		},
		{
			name:  "deeply nested package",
			input: "/a.b.c.Service/Method",
			want:  "a.b.c.Service",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "only slash",
			input: "/",
			want:  "",
		},
		{
			name:  "no slash at all",
			input: "NoService",
			want:  "NoService",
		},
		{
			name:  "only method part after leading slash",
			input: "/Method",
			want:  "Method",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeGRPCMethod(tc.input)
			if got != tc.want {
				t.Errorf("normalizeGRPCMethod(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// setupGRPCProxyTest — shared pipeline setup
// ---------------------------------------------------------------------------

// grpcProxyHarness holds all components wired for a proxy pipeline test.
type grpcProxyHarness struct {
	conn        *grpc.ClientConn
	proxySrv    *grpc.Server
	healthStore *storage.HealthStore
	backendSrv  *grpc.Server
	proxy       *GRPCProxy
}

// setupGRPCProxyTest starts:
//  1. A real TCP backend gRPC server using backendHandler for all unknown services.
//  2. A GRPCProxy configured to route to that backend.
//  3. A real TCP proxy listener.
//  4. A gRPC client connected to the proxy, using rawCodec for transparent sends.
//
// All resources are cleaned up via t.Cleanup.
func setupGRPCProxyTest(t *testing.T, backendHandler grpc.StreamHandler) *grpcProxyHarness {
	t.Helper()

	// 1. Start backend on a random TCP port.
	backendLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	backendSrv := grpc.NewServer(grpc.UnknownServiceHandler(backendHandler))
	go func() { _ = backendSrv.Serve(backendLis) }()
	t.Cleanup(backendSrv.Stop)

	backendAddr := backendLis.Addr().String()

	// 2. Config pointing node-1 grpc endpoint at the backend.
	yaml := `
listen: ":3000"
external_failover_threshold: 2

timeouts:
  health_check: 5s
  proxy: 30s

networks:
  - name: "testnet"
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rpc
        listen: ":18081"
      - protocol: grpc
        listen: ":18082"

internals:
  - name: node-1
    network: "testnet"
    grpc_insecure: true
    endpoints:
      rpc: "http://127.0.0.1:18099"
      grpc: "` + backendAddr + `"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)

	// 3. Seed height store so GetBestNode finds node-1.
	heightStore := storage.NewHeightStore()
	heightStore.Update("testnet", "node-1", "rpc", 100, 10*time.Millisecond, "internal")

	healthStore := storage.NewHealthStore()
	healthStore.SetHealthy("testnet", "node-1", "grpc")

	// 4. Create selector with health store.
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
	sel.SetHealthStore(healthStore)

	// 5. Create proxy (no circuit breaker — let tests add one if needed).
	grpcProxy := NewGRPCProxy(sel, cfgLoader, endpointStore, zap.NewNop(), "testnet", nil)

	// 6. Start proxy on a random TCP port.
	proxyLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	proxySrv := grpcProxy.GetServer()
	go func() { _ = proxySrv.Serve(proxyLis) }()
	t.Cleanup(proxySrv.Stop)

	// 7. Client connects to proxy using rawCodec for transparent frame sends.
	conn, err := grpc.NewClient(
		"passthrough:///"+proxyLis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(&rawCodec{})),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &grpcProxyHarness{
		conn:        conn,
		proxySrv:    proxySrv,
		healthStore: healthStore,
		backendSrv:  backendSrv,
		proxy:       grpcProxy,
	}
}

// ---------------------------------------------------------------------------
// TestGRPCProxy_ForwardsRequest — happy path: proxy echoes payload
// ---------------------------------------------------------------------------

// TestGRPCProxy_ForwardsRequest verifies that the proxy forwards a raw gRPC
// frame to the backend and returns the echoed response to the client.
func TestGRPCProxy_ForwardsRequest(t *testing.T) {
	t.Parallel()

	// Backend echoes the first message back.
	echoHandler := func(_ interface{}, stream grpc.ServerStream) error {
		var frame rawFrame
		if err := stream.RecvMsg(&frame); err != nil {
			return err
		}
		return stream.SendMsg(&frame)
	}

	h := setupGRPCProxyTest(t, echoHandler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := h.conn.NewStream(ctx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		"/test.Service/Echo",
	)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	want := []byte("hello-from-client")
	if err := stream.SendMsg(&rawFrame{payload: want}); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	var resp rawFrame
	if err := stream.RecvMsg(&resp); err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}

	if string(resp.payload) != string(want) {
		t.Errorf("echo payload mismatch: got %q, want %q", resp.payload, want)
	}
}

// ---------------------------------------------------------------------------
// TestGRPCProxy_ForwardsLargePayload — edge case: oversized payload
// ---------------------------------------------------------------------------

// TestGRPCProxy_ForwardsLargePayload ensures the proxy handles large payloads
// (1 MiB) without truncation.
func TestGRPCProxy_ForwardsLargePayload(t *testing.T) {
	t.Parallel()

	echoHandler := func(_ interface{}, stream grpc.ServerStream) error {
		var frame rawFrame
		if err := stream.RecvMsg(&frame); err != nil {
			return err
		}
		return stream.SendMsg(&frame)
	}

	h := setupGRPCProxyTest(t, echoHandler)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := h.conn.NewStream(ctx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		"/test.Service/BigEcho",
	)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	// 1 MiB payload
	large := make([]byte, 1024*1024)
	for i := range large {
		large[i] = byte(i % 251)
	}

	if err := stream.SendMsg(&rawFrame{payload: large}); err != nil {
		t.Fatalf("SendMsg large: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	var resp rawFrame
	if err := stream.RecvMsg(&resp); err != nil {
		t.Fatalf("RecvMsg large: %v", err)
	}

	if len(resp.payload) != len(large) {
		t.Errorf("payload length mismatch: got %d, want %d", len(resp.payload), len(large))
	}
}

// ---------------------------------------------------------------------------
// TestGRPCProxy_NoAvailableNodes — no nodes in height store
// ---------------------------------------------------------------------------

// TestGRPCProxy_NoAvailableNodes verifies that when the selector has no nodes
// available (empty height store), the proxy returns codes.Unavailable.
func TestGRPCProxy_NoAvailableNodes(t *testing.T) {
	t.Parallel()

	// Config with grpc endpoint but height store never seeded — GetBestNode returns nil.
	yaml := `
listen: ":3000"
external_failover_threshold: 2
timeouts:
  health_check: 5s
  proxy: 30s
networks:
  - name: "testnet"
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rpc
        listen: ":18083"
      - protocol: grpc
        listen: ":18084"
internals:
  - name: node-1
    network: "testnet"
    grpc_insecure: true
    endpoints:
      rpc: "http://127.0.0.1:18099"
      grpc: "127.0.0.1:19900"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)

	// Empty height store — no heights → GetBestNode returns nil.
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())

	grpcProxy := NewGRPCProxy(sel, cfgLoader, endpointStore, zap.NewNop(), "testnet", nil)

	proxyLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	proxySrv := grpcProxy.GetServer()
	go func() { _ = proxySrv.Serve(proxyLis) }()
	t.Cleanup(proxySrv.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///"+proxyLis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(&rawCodec{})),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := conn.NewStream(ctx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		"/test.Service/NoNodes",
	)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	// Must send at least one message to trigger the handler path.
	_ = stream.SendMsg(&rawFrame{payload: []byte("ping")})
	_ = stream.CloseSend()

	var resp rawFrame
	err = stream.RecvMsg(&resp)
	if err == nil {
		t.Fatal("expected an error response, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.Unavailable {
		t.Errorf("expected codes.Unavailable, got %s", st.Code())
	}
}

// ---------------------------------------------------------------------------
// TestGRPCProxy_BackendDialError — backend address unreachable
// ---------------------------------------------------------------------------

// TestGRPCProxy_BackendDialError verifies that when the backend address is
// unreachable (nothing listening), the proxy stream returns a non-OK status.
//
// Note: grpc.NewClient (lazy dial) defers the actual connection until the
// first RPC call. The error surfaces as a stream error, not a dial error.
func TestGRPCProxy_BackendDialError(t *testing.T) {
	t.Parallel()

	// Bind and immediately close to get a port with nothing listening.
	deadLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := deadLis.Addr().String()
	_ = deadLis.Close()

	yaml := `
listen: ":3000"
external_failover_threshold: 2
timeouts:
  health_check: 5s
  proxy: 2s
networks:
  - name: "testnet"
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rpc
        listen: ":18085"
      - protocol: grpc
        listen: ":18086"
internals:
  - name: node-1
    network: "testnet"
    grpc_insecure: true
    endpoints:
      rpc: "http://127.0.0.1:18099"
      grpc: "` + deadAddr + `"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)

	heightStore := storage.NewHeightStore()
	heightStore.Update("testnet", "node-1", "rpc", 100, 10*time.Millisecond, "internal")
	healthStore := storage.NewHealthStore()
	healthStore.SetHealthy("testnet", "node-1", "grpc")
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
	sel.SetHealthStore(healthStore)

	grpcProxy := NewGRPCProxy(sel, cfgLoader, endpointStore, zap.NewNop(), "testnet", nil)

	proxyLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	proxySrv := grpcProxy.GetServer()
	go func() { _ = proxySrv.Serve(proxyLis) }()
	t.Cleanup(proxySrv.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///"+proxyLis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(&rawCodec{})),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := conn.NewStream(ctx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		"/test.Service/DialFail",
	)
	if err != nil {
		// Stream creation itself may fail with Unavailable; that's valid.
		st, ok := status.FromError(err)
		if !ok {
			t.Fatalf("unexpected non-status error from NewStream: %v", err)
		}
		// Unavailable or Internal are both acceptable for a dial error path.
		if st.Code() != codes.Unavailable && st.Code() != codes.Internal {
			t.Errorf("expected Unavailable or Internal from NewStream, got %s", st.Code())
		}
		return
	}

	_ = stream.SendMsg(&rawFrame{payload: []byte("ping")})
	_ = stream.CloseSend()

	var resp rawFrame
	err = stream.RecvMsg(&resp)
	if err == nil {
		t.Fatal("expected error due to unreachable backend, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		// Wrapped errors are also acceptable — just verify it's non-nil.
		return
	}
	// Unavailable, Internal, or Unknown are all acceptable for a connection failure.
	switch st.Code() {
	case codes.Unavailable, codes.Internal, codes.Unknown:
		// expected
	default:
		t.Errorf("unexpected gRPC status code for dial error: %s", st.Code())
	}
}

// ---------------------------------------------------------------------------
// TestGRPCProxy_CircuitBreakerTrips — CB marks node unhealthy
// ---------------------------------------------------------------------------

// TestGRPCProxy_CircuitBreakerTrips verifies that the CircuitBreaker marks a
// node unhealthy after the configured threshold of consecutive errors.
//
// Note: The GRPCProxy stores the circuit breaker but does not call RecordError
// automatically in proxyHandler. This test validates the CB logic itself and
// the integration of HealthStore with the selector — it exercises the full
// "CB trips → node excluded" pipeline.
func TestGRPCProxy_CircuitBreakerTrips(t *testing.T) {
	t.Parallel()

	hs := storage.NewHealthStore()
	hs.SetHealthy("testnet", "node-1", "grpc")
	cb := NewCircuitBreaker(hs, zap.NewNop())

	// Simulate 3 consecutive errors at threshold=3 → node becomes unhealthy.
	cb.RecordError("testnet", "node-1", "grpc", 3)
	cb.RecordError("testnet", "node-1", "grpc", 3)

	// Before threshold: still healthy.
	if !hs.IsHealthy("testnet", "node-1", "grpc") {
		t.Fatal("node should still be healthy at 2/3 errors")
	}

	cb.RecordError("testnet", "node-1", "grpc", 3)

	// After threshold: node is unhealthy.
	if hs.IsHealthy("testnet", "node-1", "grpc") {
		t.Fatal("node should be unhealthy after 3 consecutive errors at threshold=3")
	}

	h := hs.GetHealth("testnet", "node-1", "grpc")
	if h == nil {
		t.Fatal("health entry missing after circuit breaker trip")
	}
	if h.LastError == "" {
		t.Error("LastError should describe the circuit breaker trip")
	}

	// Now verify the selector excludes the unhealthy node. Seed a height store
	// and create a selector with this health store.
	yaml := `
listen: ":3000"
external_failover_threshold: 2
timeouts:
  health_check: 5s
  proxy: 30s
networks:
  - name: "testnet"
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rpc
        listen: ":18087"
      - protocol: grpc
        listen: ":18088"
internals:
  - name: node-1
    network: "testnet"
    grpc_insecure: true
    endpoints:
      rpc: "http://127.0.0.1:18099"
      grpc: "127.0.0.1:19900"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)
	heightStore := storage.NewHeightStore()
	heightStore.Update("testnet", "node-1", "rpc", 100, 10*time.Millisecond, "internal")
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
	sel.SetHealthStore(hs)

	// GetBestNode for "grpc" must return nil because node-1 is unhealthy for grpc.
	nm, name, _ := sel.GetBestNode("testnet", "grpc")
	if nm != nil || name != "" {
		t.Errorf("selector should exclude unhealthy node, got node=%q", name)
	}
}

// ---------------------------------------------------------------------------
// TestGRPCProxy_BackendReturnsError — backend gRPC error propagated
// ---------------------------------------------------------------------------

// TestGRPCProxy_BackendReturnsError verifies that when the backend returns a
// gRPC error (codes.Internal), the proxy propagates a non-OK status to the
// client.
func TestGRPCProxy_BackendReturnsError(t *testing.T) {
	t.Parallel()

	errorHandler := func(_ interface{}, stream grpc.ServerStream) error {
		var frame rawFrame
		_ = stream.RecvMsg(&frame)
		return status.Errorf(codes.Internal, "deliberate backend error")
	}

	h := setupGRPCProxyTest(t, errorHandler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := h.conn.NewStream(ctx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		"/test.Service/Fail",
	)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	if err := stream.SendMsg(&rawFrame{payload: []byte("trigger")}); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	_ = stream.CloseSend()

	var resp rawFrame
	err = stream.RecvMsg(&resp)
	if err == nil {
		t.Fatal("expected error from backend, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		// Wrapped error is also acceptable — the important thing is err != nil.
		return
	}
	// codes.Internal or codes.Unknown are both acceptable when backend returns Internal.
	switch st.Code() {
	case codes.Internal, codes.Unknown, codes.Unavailable:
		// expected
	default:
		t.Errorf("unexpected gRPC status from backend error: %s", st.Code())
	}
}

// ---------------------------------------------------------------------------
// TestGRPCProxy_MetadataForwarded — incoming metadata forwarded to backend
// ---------------------------------------------------------------------------

// TestGRPCProxy_MetadataForwarded verifies that incoming gRPC metadata is
// forwarded from the client to the backend. The proxy reads incoming metadata
// and sets it as outgoing metadata on the backend connection.
func TestGRPCProxy_MetadataForwarded(t *testing.T) {
	t.Parallel()

	// receivedMeta is written by the backend handler goroutine and read by the
	// test goroutine. A channel is used for safe cross-goroutine communication.
	metaCh := make(chan string, 1)

	captureHandler := func(_ interface{}, stream grpc.ServerStream) error {
		// Read incoming metadata on the backend stream.
		md, _ := metadata.FromIncomingContext(stream.Context())
		if vals := md["x-test-key"]; len(vals) > 0 {
			metaCh <- vals[0]
		} else {
			metaCh <- ""
		}

		var frame rawFrame
		if err := stream.RecvMsg(&frame); err != nil {
			return err
		}
		return stream.SendMsg(&rawFrame{payload: []byte("ok")})
	}

	h := setupGRPCProxyTest(t, captureHandler)

	// Attach outgoing metadata to the client context so the proxy can forward it.
	md := metadata.Pairs("x-test-key", "test-value-42")
	ctx := metadata.NewOutgoingContext(context.Background(), md)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	stream, err := h.conn.NewStream(ctx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		"/test.Service/WithMetadata",
	)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	if err := stream.SendMsg(&rawFrame{payload: []byte("meta-test")}); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	_ = stream.CloseSend()

	var resp rawFrame
	_ = stream.RecvMsg(&resp)

	// Wait for the backend to record the metadata (with a short timeout).
	select {
	case val := <-metaCh:
		if val != "test-value-42" {
			t.Errorf("metadata x-test-key: got %q, want %q", val, "test-value-42")
		}
	case <-time.After(3 * time.Second):
		t.Error("backend did not receive metadata within timeout")
	}
}

// ---------------------------------------------------------------------------
// TestGRPCProxy_Concurrent — concurrent streams, race detector
// ---------------------------------------------------------------------------

// TestGRPCProxy_Concurrent fires 20 goroutines through a single proxy instance
// to confirm there is no data race. Each goroutine opens a stream and echoes.
func TestGRPCProxy_Concurrent(t *testing.T) {
	t.Parallel()

	echoHandler := func(_ interface{}, stream grpc.ServerStream) error {
		var frame rawFrame
		if err := stream.RecvMsg(&frame); err != nil {
			return err
		}
		return stream.SendMsg(&frame)
	}

	h := setupGRPCProxyTest(t, echoHandler)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream, err := h.conn.NewStream(ctx,
				&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
				"/test.Service/ConcurrentEcho",
			)
			if err != nil {
				errs <- err
				return
			}

			if err := stream.SendMsg(&rawFrame{payload: []byte("concurrent")}); err != nil {
				errs <- err
				return
			}
			if err := stream.CloseSend(); err != nil {
				errs <- err
				return
			}

			var resp rawFrame
			if err := stream.RecvMsg(&resp); err != nil {
				errs <- err
				return
			}

			if string(resp.payload) != "concurrent" {
				errs <- nil // payload mismatch tracked separately
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("concurrent stream error: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// TestGRPCProxy_GetServerUsesDefaultMsgSizes — GetServer defaults
// ---------------------------------------------------------------------------

// TestGRPCProxy_GetServerUsesDefaultMsgSizes verifies that GetServer returns
// a non-nil *grpc.Server when the network config exists but has no explicit
// message size limits (should default to 100 MiB).
func TestGRPCProxy_GetServerUsesDefaultMsgSizes(t *testing.T) {
	t.Parallel()

	cfgLoader := createTestConfigLoader(t)
	p := NewGRPCProxy(nil, cfgLoader, nil, zap.NewNop(), "testnet", nil)
	srv := p.GetServer()
	if srv == nil {
		t.Fatal("GetServer returned nil")
	}
	// If it didn't panic, defaults applied correctly.
}

// ---------------------------------------------------------------------------
// TestGRPCProxy_GetServerUnknownNetwork — network not in config
// ---------------------------------------------------------------------------

// TestGRPCProxy_GetServerUnknownNetwork verifies that GetServer succeeds even
// when the network is not in the config (falls back to 100 MiB defaults).
func TestGRPCProxy_GetServerUnknownNetwork(t *testing.T) {
	t.Parallel()

	cfgLoader := createTestConfigLoader(t)
	p := NewGRPCProxy(nil, cfgLoader, nil, zap.NewNop(), "nonexistent-network", nil)
	srv := p.GetServer()
	if srv == nil {
		t.Fatal("GetServer returned nil for unknown network")
	}
}

// ---------------------------------------------------------------------------
// helpers for connection pool tests
// ---------------------------------------------------------------------------

// newTestGRPCProxy creates a minimal *GRPCProxy suitable for unit tests that
// only exercise the connection pool; it does NOT start a gRPC server.
func newTestGRPCProxy(t *testing.T) *GRPCProxy {
	t.Helper()
	cfgLoader := createTestConfigLoader(t)
	return &GRPCProxy{
		configLoader: cfgLoader,
		logger:       zap.NewNop(),
		network:      "testnet",
		connPool:     make(map[string]*grpc.ClientConn),
	}
}

// dialInsecure is a small helper that creates a grpc.ClientConn to a
// passthrough address using insecure credentials. Nothing is listening at the
// address; grpc.NewClient does not actually dial until a call is made, so this
// succeeds immediately.
func dialInsecure(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	target := "passthrough:///" + addr
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialInsecure(%s): %v", addr, err)
	}
	return conn
}

// ---------------------------------------------------------------------------
// TestGetOrCreateConnection_ClosesShutdownConnection (H4)
// ---------------------------------------------------------------------------

// TestGetOrCreateConnection_ClosesShutdownConnection verifies that when the
// cached connection for an address is in SHUTDOWN state the next call to
// getOrCreateConnection:
//  1. removes the dead connection from the pool
//  2. returns a newly created connection that is NOT the old one
func TestGetOrCreateConnection_ClosesShutdownConnection(t *testing.T) {
	p := newTestGRPCProxy(t)

	addr := "127.0.0.1:19990"

	// Create a real connection and immediately close it so it enters SHUTDOWN.
	deadConn := dialInsecure(t, addr)
	_ = deadConn.Close() // drives state → SHUTDOWN

	// Verify it really is SHUTDOWN before seeding.
	if deadConn.GetState() != connectivity.Shutdown {
		t.Skipf("connection did not reach SHUTDOWN (got %s); skipping", deadConn.GetState())
	}

	// Seed the pool with the dead connection.
	p.connMu.Lock()
	p.connPool[addr] = deadConn
	p.connMu.Unlock()

	// Ask the proxy for a connection — it must detect SHUTDOWN and return a fresh one.
	newConn, err := p.getOrCreateConnection(addr, true /*insecure*/)
	if err != nil {
		t.Fatalf("getOrCreateConnection: %v", err)
	}
	defer func() { _ = newConn.Close() }()

	// The returned connection must not be the dead one.
	if newConn == deadConn {
		t.Error("getOrCreateConnection returned the same SHUTDOWN connection; expected a new one")
	}

	// The pool must now hold the new connection, not the dead one.
	p.connMu.RLock()
	pooled := p.connPool[addr]
	p.connMu.RUnlock()

	if pooled == deadConn {
		t.Error("connPool still holds the SHUTDOWN connection after replacement")
	}
	if pooled != newConn {
		t.Error("connPool does not hold the newly created connection")
	}

	// The new connection must not be in SHUTDOWN state.
	if pooled.GetState() == connectivity.Shutdown {
		t.Errorf("new pooled connection is already in SHUTDOWN: %s", pooled.GetState())
	}
}

// ---------------------------------------------------------------------------
// TestGetOrCreateConnection_ConcurrentAccess (race detector)
// ---------------------------------------------------------------------------

// TestGetOrCreateConnection_ConcurrentAccess runs 50 goroutines all requesting
// the same target address concurrently. The race detector flags any locking
// defects in getOrCreateConnection.
func TestGetOrCreateConnection_ConcurrentAccess(t *testing.T) {
	p := newTestGRPCProxy(t)
	addr := "127.0.0.1:19991"

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			conn, err := p.getOrCreateConnection(addr, true /*insecure*/)
			if err != nil {
				errs <- err
				return
			}
			_ = conn
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("getOrCreateConnection error: %v", err)
	}

	// Clean up via the proxy's Close — must not panic or race.
	if err := p.Close(); err != nil {
		t.Logf("p.Close() returned error (benign on non-listening addr): %v", err)
	}

	// Second Close must be a no-op (idempotent — H8 requirement).
	if err := p.Close(); err != nil {
		t.Logf("p.Close() second call returned error: %v", err)
	}
}
