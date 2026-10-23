package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"sauron/internal/testutil"
	"sauron/metrics"
	"sauron/selector"
	"sauron/storage"

	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
)

// newCBHTTPProxy wires an HTTPProxy with a circuit breaker (threshold 100,
// 2 retries) in front of backendURL for network/type rest. The height store is
// returned so a test can mark the node's WebSocket support.
func newCBHTTPProxy(t *testing.T, network, backendURL string) (*HTTPProxy, *CircuitBreaker, *storage.HeightStore) {
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
	return NewHTTPProxy(sel, cfgLoader, endpointStore, zap.NewNop(), "rest", network, cb), cb, heightStore
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

	p, cb, _ := newCBHTTPProxy(t, "cancel-net", backend.URL)

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

	p, _, _ := newCBHTTPProxy(t, "body-net", backend.URL)

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
func TestProxyWebSocket_NonSwitchingResponseIsBackendError(t *testing.T) {
	t.Parallel()

	for _, code := range []int{http.StatusServiceUnavailable, http.StatusBadRequest, http.StatusOK} {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		resultCh := make(chan error, 1)
		statusCh := make(chan int, 1)
		proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s, err := ProxyWebSocket(w, r, backend.URL, zap.NewNop(), 0)
			statusCh <- s
			resultCh <- err
		}))

		req, _ := http.NewRequest(http.MethodGet, proxySrv.URL+"/websocket", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("backend %d: request: %v", code, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != code {
			t.Errorf("backend %d: client got %d, want the backend's status", code, resp.StatusCode)
		}

		select {
		case err := <-resultCh:
			gotStatus := <-statusCh
			var wsErr *WSProxyError
			if !errors.As(err, &wsErr) || wsErr.Category != "upgrade_rejected" {
				t.Errorf("backend %d: err = %v, want WSProxyError category upgrade_rejected", code, err)
			}
			if gotStatus != code {
				t.Errorf("backend %d: returned status %d", code, gotStatus)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("backend %d: ProxyWebSocket did not return (copy loop started on a refused upgrade)", code)
		}
		proxySrv.Close()
		backend.Close()
	}
}

// TestHTTPProxy_WebSocketRejectionCountsOnlyBreakerCodes: a refused upgrade
// counts against the node only when its status is one the circuit breaker is
// configured for. A 400 answers a bad client handshake and says nothing about
// the node; counting it would let any client open a healthy node's breaker.
func TestHTTPProxy_WebSocketRejectionCountsOnlyBreakerCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		network     string
		backendCode int
		wantCBErrs  int
	}{
		{name: "breaker code counts", network: "ws-503-net", backendCode: http.StatusServiceUnavailable, wantCBErrs: 1},
		{name: "client error does not count", network: "ws-400-net", backendCode: http.StatusBadRequest, wantCBErrs: 0},
		{name: "non-breaker 5xx does not count", network: "ws-500-net", backendCode: http.StatusInternalServerError, wantCBErrs: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.backendCode)
			}))
			defer backend.Close()

			p, cb, heights := newCBHTTPProxy(t, tt.network, backend.URL)
			heights.UpdateWebSocketAvailability(tt.network, "node-1", "rpc", true)
			proxySrv := httptest.NewServer(p)
			defer proxySrv.Close()

			rejected := metrics.ProxyErrors.WithLabelValues(tt.network, "node-1", "rest", strconv.Itoa(tt.backendCode), "upgrade_rejected")
			rejectedCount := func() float64 {
				var m dto.Metric
				_ = rejected.Write(&m)
				return m.GetCounter().GetValue()
			}
			rejectedBefore := rejectedCount()

			req, _ := http.NewRequest(http.MethodGet, proxySrv.URL+"/websocket", nil)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tt.backendCode {
				t.Errorf("client got %d, want the backend's %d", resp.StatusCode, tt.backendCode)
			}
			// The client has its answer before the handler records the outcome:
			// wait for the upgrade_rejected metric, which comes right before the
			// breaker decision, then for the breaker count.
			deadline := time.Now().Add(2 * time.Second)
			for {
				recorded := rejectedCount() - rejectedBefore
				got := cb.GetErrorCount(tt.network, "node-1", "rest")
				if recorded == 1 && got == tt.wantCBErrs {
					time.Sleep(50 * time.Millisecond) // a late, wrong increment would show now
					if got := cb.GetErrorCount(tt.network, "node-1", "rest"); got != tt.wantCBErrs {
						t.Errorf("circuit breaker errors = %d, want %d", got, tt.wantCBErrs)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("after 2s: upgrade_rejected recorded %v times, circuit breaker errors = %d, want 1 and %d", recorded, got, tt.wantCBErrs)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

// countingReader counts the bytes read from it.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func bodyTooLargeCount(network string) float64 {
	var m dto.Metric
	_ = metrics.ProxyErrors.WithLabelValues(network, "", "rest", "413", "body_too_large").Write(&m)
	return m.GetCounter().GetValue()
}

// TestHTTPProxy_OversizedBodyRejectedEarly: a declared Content-Length over the
// limit is rejected before reading the body; a chunked body (no length) is
// rejected once it passes the limit. Both are counted as body_too_large.
func TestHTTPProxy_OversizedBodyRejectedEarly(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	const network = "body-early-net"
	p, _, _ := newCBHTTPProxy(t, network, backend.URL)

	tests := []struct {
		name          string
		size          int64
		chunked       bool
		wantCode      int
		wantMaxRead   int64
		wantCountDiff float64
	}{
		{name: "declared over the limit", size: maxRequestBodySize + 1, wantCode: http.StatusRequestEntityTooLarge, wantMaxRead: 0, wantCountDiff: 1},
		{name: "chunked over the limit", size: maxRequestBodySize + 1, chunked: true, wantCode: http.StatusRequestEntityTooLarge, wantMaxRead: maxRequestBodySize + 1, wantCountDiff: 1},
		{name: "chunked at the limit", size: maxRequestBodySize, chunked: true, wantCode: http.StatusOK, wantMaxRead: maxRequestBodySize, wantCountDiff: 0},
	}
	for _, tc := range tests {
		hits.Store(0)
		before := bodyTooLargeCount(network)
		body := &countingReader{r: bytes.NewReader(make([]byte, tc.size))}
		req := httptest.NewRequest(http.MethodPost, "/", body)
		req.ContentLength = tc.size
		if tc.chunked {
			req.ContentLength = -1
		}
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)

		if rr.Code != tc.wantCode {
			t.Errorf("%s: status = %d, want %d", tc.name, rr.Code, tc.wantCode)
		}
		if got := body.n.Load(); got > tc.wantMaxRead {
			t.Errorf("%s: read %d body bytes, want at most %d", tc.name, got, tc.wantMaxRead)
		}
		wantHits := int32(0)
		if tc.wantCode == http.StatusOK {
			wantHits = 1
		}
		if got := hits.Load(); got != wantHits {
			t.Errorf("%s: backend hits = %d, want %d", tc.name, got, wantHits)
		}
		if got := bodyTooLargeCount(network) - before; got != tc.wantCountDiff {
			t.Errorf("%s: body_too_large count +%v, want +%v", tc.name, got, tc.wantCountDiff)
		}
	}
}

// A refused upgrade whose body has no length (read until close) must not hang
// the handler on a backend that keeps the connection open.
func TestProxyWebSocket_RejectionBodyWithoutLengthDoesNotHang(t *testing.T) {
	t.Parallel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		c, err := lis.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		buf := make([]byte, 4096)
		_, _ = c.Read(buf) // the upgrade request
		_, _ = c.Write([]byte("HTTP/1.1 503 Service Unavailable\r\nContent-Type: text/plain\r\n\r\nnode is starting"))
		time.Sleep(10 * time.Second) // keep the connection open
	}()

	resultCh := make(chan error, 1)
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := ProxyWebSocket(w, r, "http://"+lis.Addr().String(), zap.NewNop(), 0)
		resultCh <- err
	}))
	t.Cleanup(proxySrv.Close)

	go func() {
		req, _ := http.NewRequest(http.MethodGet, proxySrv.URL+"/websocket", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	select {
	case err := <-resultCh:
		var wsErr *WSProxyError
		if !errors.As(err, &wsErr) || wsErr.Category != "upgrade_rejected" || wsErr.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("err = %v, want upgrade_rejected with 503", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ProxyWebSocket still blocked 5s after a refused upgrade with an open-ended body")
	}
}

// A percent-encoded slash (%2F) in the request path or in the node URL's path
// reaches the node still encoded: decoding it changes which route matches.
// The join follows httputil.NewSingleHostReverseProxy (joinURLPath).
func TestHTTPProxy_EncodedSlashReachesNodeEncoded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, targetPath, reqPath, want string
	}{
		{name: "plain path", targetPath: "", reqPath: "/a/b", want: "/a/b"},
		{name: "encoded request, no target path", targetPath: "", reqPath: "/a%2Fb", want: "/a%2Fb"},
		{name: "encoded request under a target path", targetPath: "/base", reqPath: "/a%2Fb", want: "/base/a%2Fb"},
		{name: "encoded target path", targetPath: "/base%2Fx", reqPath: "/a", want: "/base%2Fx/a"},
		{name: "both encoded", targetPath: "/base%2Fx", reqPath: "/a%2Fb", want: "/base%2Fx/a%2Fb"},
		{name: "query kept", targetPath: "", reqPath: "/a%2Fb?height=5", want: "/a%2Fb?height=5"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := make(chan string, 1)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got <- r.RequestURI
				w.WriteHeader(http.StatusOK)
			}))
			defer backend.Close()

			p, _, _ := newCBHTTPProxy(t, "encoded-net-"+strconv.Itoa(i), backend.URL+tt.targetPath)
			rr := httptest.NewRecorder()
			p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tt.reqPath, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status %d", rr.Code)
			}
			if uri := <-got; uri != tt.want {
				t.Errorf("node received %q, want %q", uri, tt.want)
			}
		})
	}
}
