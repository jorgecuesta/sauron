package checker

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sauron/adapter"
	"sauron/adapter/cosmos"
	"sauron/config"
	"sauron/internal/testutil"
	"sauron/storage"

	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// testMultiChainSetupWithArchival creates a scheduler that has an archival
// network configured, pointing nodes at nodeURL.
func testMultiChainSetupWithArchival(t *testing.T, nodeURL string) (*MultiChainScheduler, *storage.ArchivalStore) {
	t.Helper()

	heightStore := storage.NewHeightStore()
	healthStore := storage.NewHealthStore()
	archivalStore := storage.NewArchivalStore()
	cache := storage.NewCache("", zap.NewNop())
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())

	registry := adapter.NewRegistry()
	if err := registry.Register(cosmos.New()); err != nil {
		t.Fatalf("register cosmos: %v", err)
	}

	engine := adapter.NewEngine(&http.Client{Timeout: 2 * time.Second})

	cfgYAML := `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rest
        listen: ":18080"
      - protocol: rpc
        listen: ":18081"
    archival:
      min_height: 1
      check_interval: 30s
internals:
  - name: seed-one
    network: pocket
    endpoints:
      rest: "` + nodeURL + `"
      rpc: "` + nodeURL + `"
`
	loader := testutil.NewMultiChainLoader(t, cfgYAML)
	pool := newTestPool()

	sched := NewMultiChainScheduler(engine, registry, heightStore, healthStore, archivalStore, cache, endpointStore, nil, loader, pool, zap.NewNop())
	return sched, archivalStore
}

// testMultiChainSetup creates all dependencies for MultiChainScheduler tests.
func testMultiChainSetup(t *testing.T, nodeURL string) (*MultiChainScheduler, *storage.HeightStore, *storage.HealthStore) {
	t.Helper()

	heightStore := storage.NewHeightStore()
	healthStore := storage.NewHealthStore()
	cache := storage.NewCache("", zap.NewNop())
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())

	registry := adapter.NewRegistry()
	if err := registry.Register(cosmos.New()); err != nil {
		t.Fatalf("register cosmos: %v", err)
	}

	engine := adapter.NewEngine(&http.Client{Timeout: 2 * time.Second})

	// Write temp V2 config.
	cfgYAML := `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rest
        listen: ":18080"
      - protocol: rpc
        listen: ":18081"
internals:
  - name: seed-one
    network: pocket
    endpoints:
      rest: "` + nodeURL + `"
      rpc: "` + nodeURL + `"
`
	loader := testutil.NewMultiChainLoader(t, cfgYAML)

	// Use a synchronous pool (size 1) for test determinism.
	pool := newTestPool()

	sched := NewMultiChainScheduler(engine, registry, heightStore, healthStore, nil, cache, endpointStore, nil, loader, pool, zap.NewNop())

	return sched, heightStore, healthStore
}

func TestMultiChainScheduler_CheckInternalNodes_HeightCheck(t *testing.T) {
	t.Parallel()

	// Mock a Cosmos RPC /status response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"result":{"sync_info":{"latest_block_height":"58272941"}}}`))
		case "/cosmos/base/tendermint/v1beta1/blocks/latest":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"sdk_block":{"header":{"height":"58272941"}}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	sched, heightStore, healthStore := testMultiChainSetup(t, srv.URL)

	// Run height checks directly.
	sched.checkInternalNodes()

	// Wait for pool to drain.
	drainTestPool(sched.pool)

	// Verify height was recorded.
	nodes := heightStore.GetByNetwork("pocket", "rpc")
	if len(nodes) == 0 {
		t.Fatal("expected height to be recorded for seed-one rpc")
	}
	found := false
	for _, n := range nodes {
		if n.Height == 58272941 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected height 58272941, got nodes: %+v", nodes)
	}

	// Verify health was recorded.
	if !healthStore.IsHealthy("pocket", "seed-one", "rpc") {
		t.Fatal("expected seed-one rpc to be healthy")
	}
}

func TestMultiChainScheduler_CheckInternalNodes_HeightCheckFailed(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	sched, _, healthStore := testMultiChainSetup(t, srv.URL)

	sched.checkInternalNodes()
	drainTestPool(sched.pool)

	if healthStore.IsHealthy("pocket", "seed-one", "rpc") {
		t.Fatal("expected seed-one rpc to be unhealthy after 500")
	}

	h := healthStore.GetHealth("pocket", "seed-one", "rpc")
	if h == nil {
		t.Fatal("expected health entry to exist")
	}
	if h.LastError == "" {
		t.Fatal("expected LastError to be set")
	}
}

func TestMultiChainScheduler_CheckInternalNodes_HealthCheck(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"result":{"sync_info":{"latest_block_height":"100"}}}`))
		case "/cosmos/base/tendermint/v1beta1/blocks/latest":
			// REST health check — return 200.
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"sdk_block":{"header":{"height":"100"}}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	sched, _, healthStore := testMultiChainSetup(t, srv.URL)

	sched.checkInternalNodes()
	drainTestPool(sched.pool)

	// REST health check should pass (separate from RPC height check).
	if !healthStore.IsHealthy("pocket", "seed-one", "rest") {
		t.Fatal("expected seed-one rest to be healthy")
	}
}

func TestMultiChainScheduler_CheckInternalNodes_HealthCheckFailed(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"result":{"sync_info":{"latest_block_height":"100"}}}`))
		case "/cosmos/base/tendermint/v1beta1/blocks/latest":
			// REST endpoint is down.
			w.WriteHeader(503)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	sched, _, healthStore := testMultiChainSetup(t, srv.URL)

	sched.checkInternalNodes()
	drainTestPool(sched.pool)

	// RPC height check should pass.
	if !healthStore.IsHealthy("pocket", "seed-one", "rpc") {
		t.Fatal("expected seed-one rpc to be healthy")
	}

	// REST health check should fail.
	if healthStore.IsHealthy("pocket", "seed-one", "rest") {
		t.Fatal("expected seed-one rest to be unhealthy")
	}
}

func TestMultiChainScheduler_StartStop(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"result":{"sync_info":{"latest_block_height":"1"}}}`))
	}))
	defer srv.Close()

	sched, _, _ := testMultiChainSetup(t, srv.URL)

	if err := sched.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Let it run briefly.
	time.Sleep(100 * time.Millisecond)

	// Stop should not panic.
	sched.Stop()
}

func TestV2NetworkToAdapterConfig(t *testing.T) {
	t.Parallel()

	net := &config.MultiChainNetwork{
		Name: "pocket",
		Type: "cosmos",
		HeightCheck: &config.HeightCheckConfig{
			Protocol: "rpc",
			Interval: 30 * time.Second,
		},
		Endpoints: []config.NetworkEndpoint{
			{Protocol: "rest", Listen: ":8080"},
			{Protocol: "rpc", Listen: ":8081"},
		},
	}
	cfg := V2NetworkToAdapterConfig(net)

	if cfg.Name != "pocket" {
		t.Fatalf("Name: got %q, want %q", cfg.Name, "pocket")
	}
	if cfg.Type != "cosmos" {
		t.Fatalf("Type: got %q, want %q", cfg.Type, "cosmos")
	}
	if cfg.HeightCheck == nil {
		t.Fatal("HeightCheck should not be nil")
	}
	if cfg.HeightCheck.Protocol != "rpc" {
		t.Fatalf("HeightCheck.Protocol: got %q, want %q", cfg.HeightCheck.Protocol, "rpc")
	}
	if len(cfg.Endpoints) != 2 {
		t.Fatalf("Endpoints: got %d, want 2", len(cfg.Endpoints))
	}
}

func TestV2NetworkToAdapterConfig_NoHeightCheck(t *testing.T) {
	t.Parallel()

	net := &config.MultiChainNetwork{
		Name: "mynet",
		Type: "evm",
		Endpoints: []config.NetworkEndpoint{
			{Protocol: "jsonrpc", Listen: ":8545"},
		},
	}
	cfg := V2NetworkToAdapterConfig(net)

	if cfg.Name != "mynet" {
		t.Fatalf("Name: got %q, want %q", cfg.Name, "mynet")
	}
	if cfg.HeightCheck != nil {
		t.Fatal("HeightCheck should be nil when not configured")
	}
	if len(cfg.Endpoints) != 1 {
		t.Fatalf("Endpoints: got %d, want 1", len(cfg.Endpoints))
	}
}

func TestV2NodeToAdapterConfig(t *testing.T) {
	t.Parallel()

	node := config.MultiChainNode{
		Name:    "seed-one",
		Network: "pocket",
		Endpoints: map[string]string{
			"rest": "http://seed-one:1317",
			"rpc":  "http://seed-one:26657",
			"grpc": "seed-one:9090",
		},
	}
	cfg := v2NodeToAdapterConfig(node)

	if cfg.Name != "seed-one" {
		t.Fatalf("Name: got %q", cfg.Name)
	}
	if cfg.Network != "pocket" {
		t.Fatalf("Network: got %q", cfg.Network)
	}
	if cfg.Endpoint["rest"] != "http://seed-one:1317" {
		t.Fatalf("rest: got %q", cfg.Endpoint["rest"])
	}
	if cfg.Endpoint["rpc"] != "http://seed-one:26657" {
		t.Fatalf("rpc: got %q", cfg.Endpoint["rpc"])
	}
	if cfg.Endpoint["grpc"] != "seed-one:9090" {
		t.Fatalf("grpc: got %q", cfg.Endpoint["grpc"])
	}
}

func TestV2NodeToAdapterConfig_Partial(t *testing.T) {
	t.Parallel()

	node := config.MultiChainNode{
		Name:    "rpc-only",
		Network: "pocket",
		Endpoints: map[string]string{
			"rpc": "http://rpc-only:26657",
		},
	}
	cfg := v2NodeToAdapterConfig(node)

	if len(cfg.Endpoint) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(cfg.Endpoint))
	}
	if cfg.Endpoint["rpc"] != "http://rpc-only:26657" {
		t.Fatalf("rpc: got %q", cfg.Endpoint["rpc"])
	}
}

func TestV2NodeEndpoints(t *testing.T) {
	t.Parallel()

	node := config.MultiChainNode{
		Endpoints: map[string]string{
			"rest": "http://api:1317",
			"rpc":  "http://rpc:26657",
			"grpc": "grpc:9090",
		},
	}

	tests := []struct {
		protocol string
		want     string
	}{
		{"rest", "http://api:1317"},
		{"rpc", "http://rpc:26657"},
		{"grpc", "grpc:9090"},
		{"jsonrpc", ""},
		{"unknown", ""},
		{"", ""},
	}

	for _, tt := range tests {
		if got := node.Endpoints[tt.protocol]; got != tt.want {
			t.Errorf("node.Endpoints[%q] = %q, want %q", tt.protocol, got, tt.want)
		}
	}
}

// testHealthCheckScheduler creates a minimal MultiChainScheduler for direct
// scheduleGRPCHealthCheck / scheduleWebSocketHealthCheck tests.
func testHealthCheckScheduler(t *testing.T) (*MultiChainScheduler, *storage.HealthStore) {
	t.Helper()

	healthStore := storage.NewHealthStore()
	pool := newTestPool()

	cfgYAML := `
listen: ":3000"
timeouts:
  health_check: 3s
  proxy: 60s
grpc_keepalive:
  time: 600s
  timeout: 20s
  permit_without_stream: true
networks:
  - name: testnet
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rpc
        listen: ":18081"
internals:
  - name: test-node
    network: testnet
    endpoints:
      rpc: "http://localhost:26657"
`
	loader := testutil.NewMultiChainLoader(t, cfgYAML)

	sched := &MultiChainScheduler{
		pool:         pool,
		healthStore:  healthStore,
		configLoader: loader,
		logger:       zap.NewNop(),
		timeout:      3 * time.Second,
	}

	return sched, healthStore
}

// TestScheduleGRPCHealthCheck_Healthy verifies that a reachable gRPC server
// results in the node being marked healthy.
func TestScheduleGRPCHealthCheck_Healthy(t *testing.T) {
	t.Parallel()

	// Start a real gRPC server on a random port.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcSrv := grpc.NewServer()
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	grpcAddr := lis.Addr().String()

	sched, healthStore := testHealthCheckScheduler(t)

	node := config.MultiChainNode{
		Name:         "test-node",
		Network:      "testnet",
		Endpoints:    map[string]string{"grpc": grpcAddr},
		GRPCInsecure: true,
	}

	sched.scheduleGRPCHealthCheck(node, grpcAddr, "grpc")
	drainTestPool(sched.pool)

	if !healthStore.IsHealthy("testnet", "test-node", "grpc") {
		h := healthStore.GetHealth("testnet", "test-node", "grpc")
		if h != nil {
			t.Fatalf("expected test-node grpc to be healthy, got LastError=%q", h.LastError)
		} else {
			t.Fatal("expected test-node grpc to be healthy, but no health entry exists")
		}
	}
}

// TestScheduleGRPCHealthCheck_Unhealthy verifies that an unreachable gRPC
// address results in the node being marked unhealthy.
func TestScheduleGRPCHealthCheck_Unhealthy(t *testing.T) {
	t.Parallel()

	// Bind and immediately close to get a port that nothing listens on.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedAddr := lis.Addr().String()
	_ = lis.Close()

	sched, healthStore := testHealthCheckScheduler(t)
	// Short timeout so the test doesn't hang on connection attempts.
	sched.timeout = 500 * time.Millisecond

	node := config.MultiChainNode{
		Name:         "test-node",
		Network:      "testnet",
		Endpoints:    map[string]string{"grpc": closedAddr},
		GRPCInsecure: true,
	}

	sched.scheduleGRPCHealthCheck(node, closedAddr, "grpc")
	drainTestPool(sched.pool)

	if healthStore.IsHealthy("testnet", "test-node", "grpc") {
		t.Fatal("expected test-node grpc to be unhealthy for closed port")
	}

	h := healthStore.GetHealth("testnet", "test-node", "grpc")
	if h == nil {
		t.Fatal("expected health entry to exist")
	}
	if h.LastError == "" {
		t.Fatal("expected LastError to be set for unreachable endpoint")
	}
}

// TestScheduleWebSocketHealthCheck_Unhealthy verifies that a plain HTTP server
// that does not support WebSocket upgrades results in the node being marked unhealthy.
func TestScheduleWebSocketHealthCheck_Unhealthy(t *testing.T) {
	t.Parallel()

	// Plain HTTP server — no WebSocket support.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not a websocket endpoint"))
	}))
	defer srv.Close()

	sched, healthStore := testHealthCheckScheduler(t)

	node := config.MultiChainNode{
		Name:      "test-node",
		Network:   "testnet",
		Endpoints: map[string]string{"rpc": srv.URL},
	}

	sched.scheduleWebSocketHealthCheck(node, srv.URL, "websocket")
	drainTestPool(sched.pool)

	if healthStore.IsHealthy("testnet", "test-node", "websocket") {
		t.Fatal("expected test-node websocket to be unhealthy for non-WS endpoint")
	}

	h := healthStore.GetHealth("testnet", "test-node", "websocket")
	if h == nil {
		t.Fatal("expected health entry to exist")
	}
	if h.LastError == "" {
		t.Fatal("expected LastError to be set")
	}
}

// TestHasArchivalNetworks_True verifies that hasArchivalNetworks returns true
// when at least one network has an archival config.
func TestHasArchivalNetworks_True(t *testing.T) {
	t.Parallel()

	cfgYAML := `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: rest
        listen: ":8080"
    archival:
      min_height: 1000
      check_interval: 30s
internals:
  - name: node-1
    network: pocket
    endpoints:
      rest: "http://localhost:1317"
`
	loader := testutil.NewMultiChainLoader(t, cfgYAML)
	sched := &MultiChainScheduler{
		configLoader: loader,
		logger:       zap.NewNop(),
	}

	if !sched.hasArchivalNetworks() {
		t.Fatal("expected hasArchivalNetworks to return true when archival is configured")
	}
}

// TestHasArchivalNetworks_False verifies that hasArchivalNetworks returns false
// when no network has an archival config.
func TestHasArchivalNetworks_False(t *testing.T) {
	t.Parallel()

	cfgYAML := `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: rest
        listen: ":8080"
internals:
  - name: node-1
    network: pocket
    endpoints:
      rest: "http://localhost:1317"
`
	loader := testutil.NewMultiChainLoader(t, cfgYAML)
	sched := &MultiChainScheduler{
		configLoader: loader,
		logger:       zap.NewNop(),
	}

	if sched.hasArchivalNetworks() {
		t.Fatal("expected hasArchivalNetworks to return false when no archival is configured")
	}
}

// TestHasArchivalNetworks_MultipleNetworks_OnlyOneArchival verifies that
// hasArchivalNetworks returns true even when only one of multiple networks has archival.
func TestHasArchivalNetworks_MultipleNetworks_OnlyOneArchival(t *testing.T) {
	t.Parallel()

	cfgYAML := `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: rest
        listen: ":8080"
  - name: pocket-beta
    type: cosmos
    endpoints:
      - protocol: rest
        listen: ":8081"
    archival:
      min_height: 500
      check_interval: 30s
internals:
  - name: node-1
    network: pocket
    endpoints:
      rest: "http://localhost:1317"
  - name: node-2
    network: pocket-beta
    endpoints:
      rest: "http://localhost:1318"
`
	loader := testutil.NewMultiChainLoader(t, cfgYAML)
	sched := &MultiChainScheduler{
		configLoader: loader,
		logger:       zap.NewNop(),
	}

	if !sched.hasArchivalNetworks() {
		t.Fatal("expected hasArchivalNetworks to return true when at least one network has archival")
	}
}

// TestCheckExternalRings_SendsRequestToMockRing verifies that checkExternalRings
// calls the external ring at /{network}/status.
func TestCheckExternalRings_SendsRequestToMockRing(t *testing.T) {
	t.Parallel()

	// Track incoming requests.
	receivedPaths := make(chan string, 10)

	// Mock ring server — responds with a valid status JSON.
	mockRing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPaths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Return a valid ExternalStatusResponse so queryRing doesn't fail.
		_, _ = w.Write([]byte(`{"height":12345,"endpoints":{}}`))
	}))
	defer mockRing.Close()

	cfgYAML := `
listen: ":3000"
timeouts:
  health_check: 2s
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: rest
        listen: ":8080"
externals:
  - name: ext-ring
    rings:
      - "` + mockRing.URL + `"
internals:
  - name: node-1
    network: pocket
    endpoints:
      rest: "http://localhost:1317"
`
	loader := testutil.NewMultiChainLoader(t, cfgYAML)

	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())

	registry := adapter.NewRegistry()
	if err := registry.Register(cosmos.New()); err != nil {
		t.Fatalf("register cosmos: %v", err)
	}
	engine := adapter.NewEngine(&http.Client{Timeout: 2 * time.Second})
	pool := newTestPool()

	sched := NewMultiChainScheduler(engine, registry, heightStore, storage.NewHealthStore(), nil,
		storage.NewCache("", zap.NewNop()), endpointStore, nil, loader, pool, zap.NewNop())

	sched.checkExternalRings()
	drainTestPool(sched.pool)

	// Verify the mock ring received a request at /pocket/status.
	select {
	case path := <-receivedPaths:
		if path != "/pocket/status" {
			t.Errorf("expected request to /pocket/status, got %q", path)
		}
	default:
		t.Fatal("expected mock ring to receive a request, but it received none")
	}
}

// TestRecoverFailedEndpoints_RecoversThenWorking verifies that recoverFailedEndpoints
// re-validates a failed endpoint when the backend now responds OK.
func TestRecoverFailedEndpoints_RecoversThenWorking(t *testing.T) {
	t.Parallel()

	// Mock server that now responds OK (simulating recovery).
	recovered := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer recovered.Close()

	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())

	// Seed a failed endpoint that points to the now-recovered server.
	endpointStore.StoreAdvertised("ext-1", "https://ring.example.com", "pocket", "rest", recovered.URL)
	// Mark validated then immediately fail it (simulates 3 proxy errors).
	endpointStore.MarkValidated("ext-1", "https://ring.example.com", "pocket", "rest", recovered.URL, 100, 10*time.Millisecond)
	endpointStore.TrackProxyError("pocket", "rest", recovered.URL)
	endpointStore.TrackProxyError("pocket", "rest", recovered.URL)
	endpointStore.TrackProxyError("pocket", "rest", recovered.URL)

	// Confirm the endpoint is in the failed state.
	failed := endpointStore.GetFailedEndpoints()
	if len(failed) != 1 {
		t.Fatalf("setup: expected 1 failed endpoint, got %d", len(failed))
	}

	cfgYAML := testutil.MinimalV2YAML
	loader := testutil.NewMultiChainLoader(t, cfgYAML)

	registry := adapter.NewRegistry()
	if err := registry.Register(cosmos.New()); err != nil {
		t.Fatalf("register cosmos: %v", err)
	}
	engine := adapter.NewEngine(&http.Client{Timeout: 2 * time.Second})
	pool := newTestPool()

	sched := NewMultiChainScheduler(engine, registry, storage.NewHeightStore(), storage.NewHealthStore(), nil,
		storage.NewCache("", zap.NewNop()), endpointStore, nil, loader, pool, zap.NewNop())

	sched.recoverFailedEndpoints()

	// After recovery, the endpoint should be validated and working.
	working := endpointStore.GetValidatedEndpoints("pocket", "rest")
	if len(working) != 1 {
		t.Fatalf("expected endpoint to be recovered and working, got %d validated endpoints", len(working))
	}
	if !working[0].IsWorking {
		t.Error("expected recovered endpoint to be working")
	}
}

// TestBuildArchivalCheckConfig_Cosmos verifies that buildArchivalCheckConfig
// returns a valid CheckConfig for a Cosmos network.
func TestBuildArchivalCheckConfig_Cosmos(t *testing.T) {
	t.Parallel()

	registry := adapter.NewRegistry()
	if err := registry.Register(cosmos.New()); err != nil {
		t.Fatalf("register cosmos: %v", err)
	}

	cfgYAML := testutil.MinimalV2YAML
	loader := testutil.NewMultiChainLoader(t, cfgYAML)

	sched := &MultiChainScheduler{
		registry:     registry,
		configLoader: loader,
		logger:       zap.NewNop(),
	}

	network := &config.MultiChainNetwork{
		Name: "pocket",
		Type: "cosmos",
		Endpoints: []config.NetworkEndpoint{
			{Protocol: "rest", Listen: ":8080"},
		},
	}

	cfg := sched.buildArchivalCheckConfig(network, 1000)
	if cfg == nil {
		t.Fatal("expected non-nil CheckConfig for cosmos archival check")
	}
	if cfg.Protocol != "rest" {
		t.Errorf("expected Protocol=rest, got %q", cfg.Protocol)
	}
	// The cosmos adapter uses /cosmos/base/tendermint/v1beta1/blocks/{height}
	if cfg.URLPath == "" {
		t.Error("expected non-empty URLPath")
	}
	if cfg.ResponsePath == "" {
		t.Error("expected non-empty ResponsePath")
	}
}

// TestBuildArchivalCheckConfig_UnknownType verifies that buildArchivalCheckConfig
// returns nil when no adapter exists for the given network type.
func TestBuildArchivalCheckConfig_UnknownType(t *testing.T) {
	t.Parallel()

	registry := adapter.NewRegistry()
	// Register only cosmos — no "unknownchain" adapter.
	if err := registry.Register(cosmos.New()); err != nil {
		t.Fatalf("register cosmos: %v", err)
	}

	cfgYAML := testutil.MinimalV2YAML
	loader := testutil.NewMultiChainLoader(t, cfgYAML)

	sched := &MultiChainScheduler{
		registry:     registry,
		configLoader: loader,
		logger:       zap.NewNop(),
	}

	network := &config.MultiChainNetwork{
		Name: "unknownnet",
		Type: "unknownchain",
	}

	cfg := sched.buildArchivalCheckConfig(network, 1000)
	if cfg != nil {
		t.Errorf("expected nil CheckConfig for unknown adapter type, got %+v", cfg)
	}
}

// TestCheckArchivalStatus_NodeMarkedArchival verifies that checkArchivalStatus
// marks a node as archival when the backend returns a valid block at min_height.
func TestCheckArchivalStatus_NodeMarkedArchival(t *testing.T) {
	t.Parallel()

	// Mock backend that returns a valid block response for the archival height check.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cosmos archival check uses /cosmos/base/tendermint/v1beta1/blocks/{height}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sdk_block":{"header":{"height":"1"}}}`))
	}))
	defer srv.Close()

	sched, archivalStore := testMultiChainSetupWithArchival(t, srv.URL)

	sched.checkArchivalStatus()
	drainTestPool(sched.pool)

	if !archivalStore.IsArchival("pocket", "seed-one") {
		t.Fatal("expected seed-one to be marked archival after successful block retrieval")
	}
}

// TestCheckArchivalStatus_NodeMarkedNotArchival verifies that checkArchivalStatus
// marks a node as non-archival when the backend returns an error.
func TestCheckArchivalStatus_NodeMarkedNotArchival(t *testing.T) {
	t.Parallel()

	// Mock backend that returns 404 (historical block not available).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"block not found"}`))
	}))
	defer srv.Close()

	sched, archivalStore := testMultiChainSetupWithArchival(t, srv.URL)

	sched.checkArchivalStatus()
	drainTestPool(sched.pool)

	status := archivalStore.GetStatus("pocket", "seed-one")
	if status == nil {
		t.Fatal("expected archival status entry to exist for seed-one")
	}
	if status.IsArchival {
		t.Fatal("expected seed-one to be marked NOT archival after 404 response")
	}
}
