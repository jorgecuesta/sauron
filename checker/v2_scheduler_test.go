package checker

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"sauron/adapter"
	"sauron/adapter/cosmos"
	"sauron/config"
	"sauron/storage"

	"go.uber.org/zap"
)

// testV2Setup creates all dependencies for V2Scheduler tests.
func testV2Setup(t *testing.T, nodeURL string) (*V2Scheduler, *storage.HeightStore, *storage.HealthStore) {
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

	// Write temp config.
	cfgYAML := `
listen: ":3000"
api: true
rpc: true
grpc: false
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: pocket
    api_listen: ":18080"
    rpc_listen: ":18081"
internals:
  - name: seed-one
    network: pocket
    api: "` + nodeURL + `"
    rpc: "` + nodeURL + `"
`
	tmpFile, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatalf("create temp config: %v", err)
	}
	if _, err := tmpFile.WriteString(cfgYAML); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatalf("close temp config: %v", err)
	}

	loader, err := config.NewLoader(tmpFile.Name(), zap.NewNop())
	if err != nil {
		t.Fatalf("create loader: %v", err)
	}

	// Use a synchronous pool (size 1) for test determinism.
	pool := newTestPool()

	sched := NewV2Scheduler(engine, registry, heightStore, healthStore, cache, endpointStore, loader, pool, zap.NewNop())

	return sched, heightStore, healthStore
}

func TestV2Scheduler_CheckInternalNodes_HeightCheck(t *testing.T) {
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

	sched, heightStore, healthStore := testV2Setup(t, srv.URL)

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

func TestV2Scheduler_CheckInternalNodes_HeightCheckFailed(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	sched, _, healthStore := testV2Setup(t, srv.URL)

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

func TestV2Scheduler_CheckInternalNodes_HealthCheck(t *testing.T) {
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

	sched, _, healthStore := testV2Setup(t, srv.URL)

	sched.checkInternalNodes()
	drainTestPool(sched.pool)

	// REST health check should pass (separate from RPC height check).
	if !healthStore.IsHealthy("pocket", "seed-one", "rest") {
		t.Fatal("expected seed-one rest to be healthy")
	}
}

func TestV2Scheduler_CheckInternalNodes_HealthCheckFailed(t *testing.T) {
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

	sched, _, healthStore := testV2Setup(t, srv.URL)

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

func TestV2Scheduler_StartStop(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"result":{"sync_info":{"latest_block_height":"1"}}}`))
	}))
	defer srv.Close()

	sched, _, _ := testV2Setup(t, srv.URL)

	if err := sched.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Let it run briefly.
	time.Sleep(100 * time.Millisecond)

	// Stop should not panic.
	sched.Stop()
}

func TestV1NetworkToAdapterConfig(t *testing.T) {
	t.Parallel()

	net := &config.Network{Name: "pocket"}
	cfg := v1NetworkToAdapterConfig(net)

	if cfg.Name != "pocket" {
		t.Fatalf("Name: got %q, want %q", cfg.Name, "pocket")
	}
	if cfg.Type != "cosmos" {
		t.Fatalf("Type: got %q, want %q", cfg.Type, "cosmos")
	}
}

func TestV1NodeToAdapterConfig(t *testing.T) {
	t.Parallel()

	node := config.Node{
		Name:    "seed-one",
		Network: "pocket",
		API:     "http://seed-one:1317",
		RPC:     "http://seed-one:26657",
		GRPC:    "seed-one:9090",
	}
	cfg := v1NodeToAdapterConfig(node)

	if cfg.Name != "seed-one" {
		t.Fatalf("Name: got %q", cfg.Name)
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

func TestV1NodeToAdapterConfig_Partial(t *testing.T) {
	t.Parallel()

	node := config.Node{
		Name:    "rpc-only",
		Network: "pocket",
		RPC:     "http://rpc-only:26657",
	}
	cfg := v1NodeToAdapterConfig(node)

	if len(cfg.Endpoint) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(cfg.Endpoint))
	}
	if cfg.Endpoint["rpc"] != "http://rpc-only:26657" {
		t.Fatalf("rpc: got %q", cfg.Endpoint["rpc"])
	}
}

func TestV1NodeURL(t *testing.T) {
	t.Parallel()

	node := config.Node{
		API:  "http://api:1317",
		RPC:  "http://rpc:26657",
		GRPC: "grpc:9090",
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
		if got := v1NodeURL(node, tt.protocol); got != tt.want {
			t.Errorf("v1NodeURL(%q) = %q, want %q", tt.protocol, got, tt.want)
		}
	}
}
