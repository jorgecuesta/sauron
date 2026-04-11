package proxy

import (
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
      rest: "http://127.0.0.1:19999"
      rpc: "http://127.0.0.1:19998"
      grpc: "127.0.0.1:19997"
`
	return testutil.NewMultiChainLoader(t, yaml)
}

// newTestProxy creates a fully-wired HTTPProxy that routes to a backend whose
// URL is passed in. The height store is seeded so GetBestNode succeeds.
// ---------------------------------------------------------------------------
// T1: TestSingleJoiningSlash
// ---------------------------------------------------------------------------

// TestSingleJoiningSlash verifies the URL path joining helper used in forwardRequest.
func TestSingleJoiningSlash(t *testing.T) {
	tests := []struct {
		a, b, want string
	}{
		{"/prefix", "/path", "/prefix/path"},
		{"/prefix/", "/path", "/prefix/path"},
		{"/prefix", "path", "/prefix/path"},
		{"/prefix/", "path", "/prefix/path"},
		{"", "/path", "/path"},
		{"/prefix", "", "/prefix/"},
	}
	for _, tt := range tests {
		got := singleJoiningSlash(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("singleJoiningSlash(%q, %q) = %q; want %q", tt.a, tt.b, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// T2: TestForwardRequestURLMerging
// ---------------------------------------------------------------------------

// TestForwardRequestURLMerging verifies that forwardRequest correctly merges
// target base URL with the incoming request's path and query string.
func TestForwardRequestURLMerging(t *testing.T) {
	// Backend that records the URL it receives.
	var receivedURL *url.URL
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedURL = r.URL
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	backendHost := backend.Listener.Addr().String()
	targetURL := "http://" + backendHost + "/prefix"

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
      - protocol: rest
        listen: ":8080"
internals:
  - name: node-1
    network: "testnet"
    endpoints:
      rest: "` + targetURL + `"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)
	heightStore := storage.NewHeightStore()
	heightStore.Update("testnet", "node-1", "rpc", 100, 10*time.Millisecond, "internal")
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
	p := NewHTTPProxy(sel, cfgLoader, endpointStore, zap.NewNop(), "rest", "testnet", nil)

	req := httptest.NewRequest("GET", "/endpoint?page=1", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if receivedURL == nil {
		t.Fatal("backend never received the request")
	}
	// Path should be joined: /prefix + /endpoint → /prefix/endpoint
	if receivedURL.Path != "/prefix/endpoint" {
		t.Errorf("path: want %q, got %q", "/prefix/endpoint", receivedURL.Path)
	}
	// Query should be preserved.
	if receivedURL.RawQuery != "page=1" {
		t.Errorf("query: want %q, got %q", "page=1", receivedURL.RawQuery)
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
      rest: "http://` + backendHost + `"
      rpc: "http://` + backendHost + `"
      grpc: "` + backendHost + `"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)

	heightStore := storage.NewHeightStore()
	heightStore.Update("testnet", "node-1", "rpc", 100, 10*time.Millisecond, "internal")

	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
	proxy := NewHTTPProxy(sel, cfgLoader, endpointStore, zap.NewNop(), "rest", "testnet", nil)

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
		endpointType: "rest",
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

// ---------------------------------------------------------------------------
// T5: TestHTTPProxy_RetryOnRetriableCode
// ---------------------------------------------------------------------------

// TestHTTPProxy_RetryOnRetriableCode verifies that when the first backend returns
// a retriable status code (503), the proxy retries to the same node (or another)
// and the client eventually gets a 200.
func TestHTTPProxy_RetryOnRetriableCode(t *testing.T) {
	t.Parallel()

	var attempts int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable) // 503 — retriable
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("success"))
	}))
	defer backend.Close()

	yaml := `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 5s
networks:
  - name: testnet
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rest
        listen: ":18080"
    circuit_breaker:
      http_codes: [429, 500, 502, 503]
      threshold: 5
      retry_attempts: 3
      retry_backoff: 10ms
internals:
  - name: node-1
    network: testnet
    endpoints:
      rest: "` + backend.URL + `"
      rpc: "` + backend.URL + `"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)
	heightStore := storage.NewHeightStore()
	heightStore.Update("testnet", "node-1", "rpc", 100, time.Millisecond, "internal")
	healthStore := storage.NewHealthStore()
	healthStore.SetHealthy("testnet", "node-1", "rest")
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
	sel.SetHealthStore(healthStore)
	cb := NewCircuitBreaker(healthStore, zap.NewNop())
	p := NewHTTPProxy(sel, cfgLoader, endpointStore, zap.NewNop(), "rest", "testnet", cb)

	req := httptest.NewRequest("GET", "/test", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 after retry, got %d", rr.Code)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts (2 fails + 1 success), got %d", attempts)
	}
}

// TestHTTPProxy_NoRetryOn400 verifies that client errors (4xx except 429)
// are NOT retried — they're the client's fault, not the node's.
func TestHTTPProxy_NoRetryOn400(t *testing.T) {
	t.Parallel()

	var attempts int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest) // 400 — not retriable
	}))
	defer backend.Close()

	yaml := `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 5s
networks:
  - name: testnet
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rest
        listen: ":18080"
    circuit_breaker:
      http_codes: [429, 500, 502, 503]
      threshold: 3
      retry_attempts: 2
      retry_backoff: 10ms
internals:
  - name: node-1
    network: testnet
    endpoints:
      rest: "` + backend.URL + `"
      rpc: "` + backend.URL + `"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)
	heightStore := storage.NewHeightStore()
	heightStore.Update("testnet", "node-1", "rpc", 100, time.Millisecond, "internal")
	healthStore := storage.NewHealthStore()
	healthStore.SetHealthy("testnet", "node-1", "rest")
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
	sel.SetHealthStore(healthStore)
	cb := NewCircuitBreaker(healthStore, zap.NewNop())
	p := NewHTTPProxy(sel, cfgLoader, endpointStore, zap.NewNop(), "rest", "testnet", cb)

	req := httptest.NewRequest("GET", "/bad", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 passed through, got %d", rr.Code)
	}
	if attempts != 1 {
		t.Errorf("expected 1 attempt (no retry on 400), got %d", attempts)
	}
}

// TestHTTPProxy_AllRetriesFail verifies that when all retries fail,
// the client gets a 502 Bad Gateway.
func TestHTTPProxy_AllRetriesFail(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // always 503
	}))
	defer backend.Close()

	yaml := `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 5s
networks:
  - name: testnet
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rest
        listen: ":18080"
    circuit_breaker:
      http_codes: [503]
      threshold: 5
      retry_attempts: 2
      retry_backoff: 10ms
internals:
  - name: node-1
    network: testnet
    endpoints:
      rest: "` + backend.URL + `"
      rpc: "` + backend.URL + `"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)
	heightStore := storage.NewHeightStore()
	heightStore.Update("testnet", "node-1", "rpc", 100, time.Millisecond, "internal")
	healthStore := storage.NewHealthStore()
	healthStore.SetHealthy("testnet", "node-1", "rest")
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
	sel.SetHealthStore(healthStore)
	cb := NewCircuitBreaker(healthStore, zap.NewNop())
	p := NewHTTPProxy(sel, cfgLoader, endpointStore, zap.NewNop(), "rest", "testnet", cb)

	req := httptest.NewRequest("GET", "/fail", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("expected 502 after all retries, got %d", rr.Code)
	}
}
