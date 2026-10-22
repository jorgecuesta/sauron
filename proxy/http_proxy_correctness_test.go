package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"sauron/internal/testutil"
	"sauron/selector"
	"sauron/storage"

	"go.uber.org/zap"
)

// newCBHTTPProxy wires an HTTPProxy with a circuit breaker (threshold 100,
// 2 retries) in front of backendURL for network/type rest.
func newCBHTTPProxy(t *testing.T, network, backendURL string) (*HTTPProxy, *CircuitBreaker) {
	t.Helper()
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
    endpoints:
      - protocol: rest
        listen: ":18080"
    circuit_breaker:
      http_codes: [502, 503]
      threshold: 100
      retry_attempts: 2
      retry_backoff: 10ms
internals:
  - name: node-1
    network: ` + network + `
    endpoints:
      rest: "` + backendURL + `"
      rpc: "` + backendURL + `"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)
	heightStore := storage.NewHeightStore()
	heightStore.Update(network, "node-1", "rpc", 100, time.Millisecond, "internal")
	healthStore := storage.NewHealthStore()
	healthStore.SetHealthy(network, "node-1", "rest")
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
	sel.SetHealthStore(healthStore)
	cb := NewCircuitBreaker(healthStore, zap.NewNop())
	return NewHTTPProxy(sel, cfgLoader, endpointStore, zap.NewNop(), "rest", network, cb), cb
}

// TestHTTPProxy_ClientCancelIsNotABackendFailure (#12): a client that gives up
// mid-request must not be retried against the node nor counted against its
// circuit breaker.
func TestHTTPProxy_ClientCancelIsNotABackendFailure(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer backend.Close()

	p, cb := newCBHTTPProxy(t, "cancel-net", backend.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/cosmos/base/tendermint/v1beta1/blocks/latest", nil).WithContext(ctx)
	p.ServeHTTP(httptest.NewRecorder(), req)

	if got := cb.GetErrorCount("cancel-net", "node-1", "rest"); got != 0 {
		t.Errorf("circuit breaker errors = %d, want 0 for a client cancel", got)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("backend hits = %d, want 1 (no retry after the client left)", got)
	}
}

// TestHTTPProxy_OversizedBodyRejected (#16): a body over the limit must be
// rejected with 413 and never forwarded truncated.
func TestHTTPProxy_OversizedBodyRejected(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, _ := newCBHTTPProxy(t, "body-net", backend.URL)

	tests := []struct {
		name     string
		size     int64
		wantCode int
		wantHits int32
	}{
		{name: "exactly at the limit", size: maxRequestBodySize, wantCode: http.StatusOK, wantHits: 1},
		{name: "one byte over the limit", size: maxRequestBodySize + 1, wantCode: http.StatusRequestEntityTooLarge, wantHits: 0},
	}
	for _, tc := range tests {
		hits.Store(0)
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(make([]byte, tc.size)))
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != tc.wantCode {
			t.Errorf("%s: status = %d, want %d", tc.name, rr.Code, tc.wantCode)
		}
		if got := hits.Load(); got != tc.wantHits {
			t.Errorf("%s: backend hits = %d, want %d", tc.name, got, tc.wantHits)
		}
	}
}

// TestProxyWebSocket_NonSwitchingResponseIsBackendError (#14): a backend that
// answers the upgrade with anything but 101 refused the WebSocket. The client
// gets that response, and the call is reported as a backend error, not a
// success that starts a copy loop.
