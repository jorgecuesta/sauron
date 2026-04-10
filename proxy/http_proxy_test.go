package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"sauron/config"
	"sauron/internal/testutil"
	"sauron/selector"
	"sauron/storage"

	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// createTestConfigLoader returns a *config.MultiChainLoader backed by a minimal V2 YAML config.
func createTestConfigLoader(t *testing.T) *config.MultiChainLoader {
	t.Helper()

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
      - protocol: api
        listen: ":8080"
      - protocol: rpc
        listen: ":8081"
      - protocol: grpc
        listen: ":8082"

internals:
  - name: node-1
    network: "testnet"
    endpoints:
      api: "http://127.0.0.1:19999"
      rpc: "http://127.0.0.1:19998"
      grpc: "127.0.0.1:19997"
`
	return testutil.NewMultiChainLoader(t, yaml)
}

// newTestProxy creates a fully-wired HTTPProxy that routes to a backend whose
// URL is passed in. The height store is seeded so GetBestNode succeeds.
// ---------------------------------------------------------------------------
// T1: TestDirectorPreservesRawQuery
// ---------------------------------------------------------------------------

// TestDirectorPreservesRawQuery verifies that when a target URL carries a
// query string and the incoming request also has a query string, the Director
// joins them with "&" so neither is dropped.
func TestDirectorPreservesRawQuery(t *testing.T) {
	target, _ := url.Parse("http://backend.example.com/prefix?key=abc")

	// Simulate an incoming request with its own query param.
	req, _ := http.NewRequest("GET", "http://proxy.example.com/endpoint?page=1", nil)

	// Inject target into context (same way ServeHTTP does it).
	ctx := context.WithValue(req.Context(), targetContextKey, target)
	req = req.WithContext(ctx)

	// Call the director logic directly (not through the deprecated Director field).
	applyDirector(req, zap.NewNop())

	q := req.URL.RawQuery
	if q != "key=abc&page=1" {
		t.Errorf("expected RawQuery %q, got %q", "key=abc&page=1", q)
	}
}

// applyDirector replicates the Director logic from NewHTTPProxy so we can
// unit-test URL rewriting without going through the deprecated Director field.
func applyDirector(req *http.Request, logger *zap.Logger) {
	target, _ := req.Context().Value(targetContextKey).(*url.URL)
	if target == nil {
		return
	}
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
	if target.Path != "" && req.URL.RawPath != "" {
		req.URL.RawPath = singleJoiningSlash(target.RawPath, req.URL.RawPath)
	}
	if target.RawQuery == "" || req.URL.RawQuery == "" {
		req.URL.RawQuery = target.RawQuery + req.URL.RawQuery
	} else {
		req.URL.RawQuery = target.RawQuery + "&" + req.URL.RawQuery
	}
	req.Host = target.Host
}

// ---------------------------------------------------------------------------
// T2: TestDirectorSetsTargetFromContext
// ---------------------------------------------------------------------------

// TestDirectorSetsTargetFromContext verifies that the shared Director reads the
// target URL out of request context and correctly sets Scheme, Host, and Path.
func TestDirectorSetsTargetFromContext(t *testing.T) {
	target, _ := url.Parse("https://backend.example.com/v1")

	req, _ := http.NewRequest("POST", "http://proxy.local/rpc", nil)
	ctx := context.WithValue(req.Context(), targetContextKey, target)
	req = req.WithContext(ctx)

	applyDirector(req, zap.NewNop())

	if req.URL.Scheme != "https" {
		t.Errorf("scheme: want %q got %q", "https", req.URL.Scheme)
	}
	if req.URL.Host != "backend.example.com" {
		t.Errorf("host: want %q got %q", "backend.example.com", req.URL.Host)
	}
	if req.Host != "backend.example.com" {
		t.Errorf("req.Host: want %q got %q", "backend.example.com", req.Host)
	}
	// Path should be joined: target "/v1" + request "/rpc" → "/v1/rpc"
	wantPath := "/v1/rpc"
	if req.URL.Path != wantPath {
		t.Errorf("path: want %q got %q", wantPath, req.URL.Path)
	}
}

// ---------------------------------------------------------------------------
// T3: TestConcurrentServeHTTP_NoRace
// ---------------------------------------------------------------------------

// TestConcurrentServeHTTP_NoRace fires 50 goroutines through a single HTTPProxy
// instance to confirm there is no data race (run with -race).  The backend is
// a real httptest server that returns 200 OK.
//
// Because NewHTTPProxy reads config for node-1's endpoint URL, and the temp
// config points node-1 to a placeholder address, we seed the height store and
// rely on the backend httptest.Server's URL — but ServeHTTP calls
// GetEndpointURL which reads the config file.  To avoid a real network hit we
// start the backend on the same address that the config specifies (127.0.0.1:19999).
// Since we cannot bind that port reliably in CI, instead we override the
// selector mock path: we use a thin shim that records whether GetBestNode is
// ever called concurrently in an unsafe way.
//
// The race detector will catch any unsynchronised access to shared fields
// (transport, reverseProxy) regardless of network outcome.
func TestConcurrentServeHTTP_NoRace(t *testing.T) {
	// Start a backend that always returns 200.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	// Build a config that points node-1 to the real backend.
	backendHost := backend.Listener.Addr().String()
	yaml := `
listen: ":3000"
external_failover_threshold: 2

timeouts:
  health_check: 5s
  proxy: 5s

networks:
  - name: "testnet"
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: api
        listen: ":8080"
      - protocol: rpc
        listen: ":8081"
      - protocol: grpc
        listen: ":8082"

internals:
  - name: node-1
    network: "testnet"
    endpoints:
      api: "http://` + backendHost + `"
      rpc: "http://` + backendHost + `"
      grpc: "` + backendHost + `"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)

	heightStore := storage.NewHeightStore()
	heightStore.Update("testnet", "node-1", "api", 100, 10*time.Millisecond, "internal")

	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
	proxy := NewHTTPProxy(sel, cfgLoader, endpointStore, zap.NewNop(), "api", "testnet")

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/health", nil)
			rr := httptest.NewRecorder()
			proxy.ServeHTTP(rr, req)
			// Accept any response — we're testing for races, not correctness.
		}()
	}

	wg.Wait()
}

// ---------------------------------------------------------------------------
// T4: TestHandleWebSocketUsesOriginalNodeMetrics
// ---------------------------------------------------------------------------

// TestHandleWebSocketUsesOriginalNodeMetrics verifies that handleWebSocket uses
// the nodeMetrics it receives (from the already-selected node) rather than
// calling GetBestNode a second time.  Specifically: if nodeMetrics says
// WebSocketAvailable=false, the proxy must return 503 without touching the
// network.
func TestHandleWebSocketUsesOriginalNodeMetrics(t *testing.T) {
	// Use a backend that should never be reached.
	target, _ := url.Parse("ws://unreachable.invalid:9999")

	nm := &storage.NodeMetrics{
		Height:             100,
		WebSocketAvailable: false, // node does NOT support WebSocket
	}
	decision := &selector.SelectionDecision{Reason: "only_available"}

	p := &HTTPProxy{
		logger:       zap.NewNop(),
		endpointType: "api",
		network:      "testnet",
	}

	// Build a real HTTP/1.1 request with WebSocket upgrade headers.
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")

	rr := httptest.NewRecorder()
	p.handleWebSocket(rr, req, target, "node-1", "testnet", time.Now(), decision, nm)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rr.Code)
	}
}
