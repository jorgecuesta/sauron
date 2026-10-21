package proxy

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sauron/internal/testutil"
	"sauron/metrics"
	"sauron/selector"
	"sauron/storage"

	"github.com/gorilla/websocket"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
)

// resetConn drops a TCP connection with RST instead of FIN, the way a killed
// process or a network failure ends it.
func resetConn(t *testing.T, c net.Conn) {
	t.Helper()
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = c.Close()
}

// wsEchoBackend echoes messages; with resetAfterFirst it RSTs the connection
// right after the first echo instead.
func wsEchoBackend(t *testing.T, resetAfterFirst bool) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, msg); err != nil {
				return
			}
			if resetAfterFirst {
				time.Sleep(50 * time.Millisecond) // let the echo reach the client first
				resetConn(t, conn.UnderlyingConn())
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// dialEcho dials url, sends one message and reads its echo.
func dialEcho(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, msg, err := conn.ReadMessage(); err != nil || string(msg) != "ping" {
		t.Fatalf("echo = %q, %v", msg, err)
	}
	return conn
}

// TestProxyWebSocket_DisconnectCategory: which side ends the session decides
// the error category. A client that drops its connection is not a backend
// failure; a backend that drops it is.
func TestProxyWebSocket_DisconnectCategory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		backendReset bool // backend RSTs after the first echo
		clientReset  bool // client RSTs after reading the echo
		wantCategory string
	}{
		{name: "client drops connection", clientReset: true, wantCategory: "client_disconnect"},
		{name: "backend drops connection", backendReset: true, wantCategory: "websocket_error"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			backend := wsEchoBackend(t, tc.backendReset)
			resultCh := make(chan error, 1)
			proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := ProxyWebSocket(w, r, backend.URL, zap.NewNop())
				resultCh <- err
			}))
			t.Cleanup(proxySrv.Close)

			conn := dialEcho(t, "ws"+proxySrv.URL[len("http"):]+"/ws")
			if tc.clientReset {
				resetConn(t, conn.UnderlyingConn())
			} else {
				defer func() { _ = conn.Close() }()
			}

			select {
			case err := <-resultCh:
				var wsErr *WSProxyError
				if !errors.As(err, &wsErr) {
					t.Fatalf("err = %v, want *WSProxyError with category %q", err, tc.wantCategory)
				}
				if wsErr.Category != tc.wantCategory {
					t.Errorf("category = %q, want %q (err: %v)", wsErr.Category, tc.wantCategory, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ProxyWebSocket did not return")
			}
		})
	}
}

// wsRequestCount reads the WEBSOCKET request counter for one network/node.
func wsRequestCount(network string) float64 {
	var m dto.Metric
	if err := metrics.NodeRequests.WithLabelValues(network, "node-1", "rest", "WEBSOCKET").Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// TestHTTPProxy_WebSocketClientDisconnectDoesNotTripBreaker: through the full
// HTTPProxy, a client dropping its WebSocket must not count against the node's
// circuit breaker, while a backend dropping it must.
func TestHTTPProxy_WebSocketClientDisconnectDoesNotTripBreaker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		network      string
		backendReset bool
		clientReset  bool
		wantCBErrors int
	}{
		{name: "client drops connection", network: "wsnet-client", clientReset: true, wantCBErrors: 0},
		{name: "backend drops connection", network: "wsnet-backend", backendReset: true, wantCBErrors: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			backend := wsEchoBackend(t, tc.backendReset)
			yaml := `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 5s
networks:
  - name: ` + tc.network + `
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rest
        listen: ":18080"
    circuit_breaker:
      http_codes: [502, 503]
      threshold: 100
      retry_attempts: 0
      retry_backoff: 10ms
internals:
  - name: node-1
    network: ` + tc.network + `
    endpoints:
      rest: "` + backend.URL + `"
      rpc: "` + backend.URL + `"
`
			cfgLoader := testutil.NewMultiChainLoader(t, yaml)
			heightStore := storage.NewHeightStore()
			heightStore.Update(tc.network, "node-1", "rpc", 100, time.Millisecond, "internal")
			heightStore.UpdateWebSocketAvailability(tc.network, "node-1", "rpc", true)
			healthStore := storage.NewHealthStore()
			healthStore.SetHealthy(tc.network, "node-1", "rest")
			endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
			sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
			sel.SetHealthStore(healthStore)
			cb := NewCircuitBreaker(healthStore, zap.NewNop())
			p := NewHTTPProxy(sel, cfgLoader, endpointStore, zap.NewNop(), "rest", tc.network, cb)

			proxySrv := httptest.NewServer(p)
			t.Cleanup(proxySrv.Close)

			// The counter is global: wait for it to move past its current value.
			baseline := wsRequestCount(tc.network)
			conn := dialEcho(t, "ws"+proxySrv.URL[len("http"):]+"/ws")
			if tc.clientReset {
				resetConn(t, conn.UnderlyingConn())
			} else {
				defer func() { _ = conn.Close() }()
			}

			deadline := time.Now().Add(5 * time.Second)
			for wsRequestCount(tc.network) < baseline+1 {
				if time.Now().After(deadline) {
					t.Fatal("WebSocket proxying never finished")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if got := cb.GetErrorCount(tc.network, "node-1", "rest"); got != tc.wantCBErrors {
				t.Errorf("circuit breaker errors = %d, want %d", got, tc.wantCBErrors)
			}
		})
	}
}
