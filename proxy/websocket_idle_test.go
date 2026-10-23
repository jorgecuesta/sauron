package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"sauron/internal/testutil"
	"sauron/selector"
	"sauron/storage"
)

// wsTickBackend upgrades and then sends a frame every interval until the
// connection ends. interval 0 means a silent backend: it upgrades, then never
// sends nor closes, like a hung node whose kernel still holds the socket.
func wsTickBackend(t *testing.T, interval time.Duration) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if interval == 0 {
			<-done
			return
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := conn.WriteMessage(websocket.TextMessage, []byte("tick")); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(func() { close(done); srv.Close() })
	return srv
}

// proxyWSOnce serves one upgrade through ProxyWebSocket with the given idle
// timeout, dials it, and returns the client connection and ProxyWebSocket's
// result channel.
func proxyWSOnce(t *testing.T, backendURL string, idle time.Duration) (*websocket.Conn, <-chan error) {
	t.Helper()
	resultCh := make(chan error, 1)
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := ProxyWebSocket(w, r, backendURL, zap.NewNop(), idle)
		resultCh <- err
	}))
	t.Cleanup(proxySrv.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+proxySrv.URL[len("http"):]+"/websocket", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, resultCh
}

// A backend that goes silent is closed after the idle timeout, with a
// category that counts against the node.
func TestProxyWebSocket_SilentBackendClosedAfterIdle(t *testing.T) {
	t.Parallel()
	backend := wsTickBackend(t, 0)
	conn, resultCh := proxyWSOnce(t, backend.URL, 200*time.Millisecond)

	select {
	case err := <-resultCh:
		var wsErr *WSProxyError
		if !errors.As(err, &wsErr) || wsErr.Category != "websocket_idle_timeout" {
			t.Fatalf("err = %v, want WSProxyError category websocket_idle_timeout", err)
		}
		if wsErr.StatusCode != http.StatusSwitchingProtocols {
			t.Errorf("StatusCode = %d, want 101", wsErr.StatusCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ProxyWebSocket still open 3s after the backend went silent (idle 200ms)")
	}

	// The client sees its connection end instead of hanging.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Error("client read a message from a silent backend")
	} else if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
		t.Error("client connection still open after the proxy gave up on the backend")
	}
}

// A backend that keeps sending (like CometBFT's ping every 27s) is never
// closed for idleness, however long the session lasts.
func TestProxyWebSocket_ActiveBackendSurvivesIdleWindows(t *testing.T) {
	t.Parallel()
	backend := wsTickBackend(t, 40*time.Millisecond)
	conn, resultCh := proxyWSOnce(t, backend.URL, 200*time.Millisecond)

	// Read for 5 idle windows; every tick must arrive.
	deadline := time.Now().Add(time.Second)
	got := 0
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read after %d ticks: %v", got, err)
		}
		if string(msg) != "tick" {
			t.Fatalf("message = %q, want tick", msg)
		}
		got++
	}
	if got < 15 {
		t.Errorf("received %d ticks in 1s at 40ms, want at least 15", got)
	}
	select {
	case err := <-resultCh:
		t.Fatalf("ProxyWebSocket ended on an active backend: %v", err)
	default:
	}
}

// Idle 0 disables the deadline: a silent backend stays connected.
func TestProxyWebSocket_IdleZeroDisablesDeadline(t *testing.T) {
	t.Parallel()
	backend := wsTickBackend(t, 0)
	_, resultCh := proxyWSOnce(t, backend.URL, 0)

	select {
	case err := <-resultCh:
		t.Fatalf("ProxyWebSocket ended with idle disabled: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestHTTPProxy_WebSocketIdleFromConfig: timeouts.websocket_idle reaches the
// proxy through production wiring, and an idle close counts against the
// node's circuit breaker.
func TestHTTPProxy_WebSocketIdleFromConfig(t *testing.T) {
	t.Parallel()

	const network = "ws-idle-net"
	backend := wsTickBackend(t, 0)
	yaml := `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 5s
  websocket_idle: 1s
networks:
  - name: ` + network + `
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rpc
        listen: ":18081"
    circuit_breaker:
      http_codes: [502, 503]
      threshold: 100
      retry_attempts: 0
      retry_backoff: 10ms
internals:
  - name: node-1
    network: ` + network + `
    endpoints:
      rpc: "` + backend.URL + `"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)
	heightStore := storage.NewHeightStore()
	heightStore.Update(network, "node-1", "rpc", 100, time.Millisecond, "internal")
	heightStore.UpdateWebSocketAvailability(network, "node-1", "rpc", true)
	healthStore := storage.NewHealthStore()
	healthStore.SetHealthy(network, "node-1", "rpc")
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
	sel.SetHealthStore(healthStore)
	cb := NewCircuitBreaker(healthStore, zap.NewNop())
	p := NewHTTPProxy(sel, cfgLoader, endpointStore, zap.NewNop(), "rpc", network, cb)
	proxySrv := httptest.NewServer(p)
	t.Cleanup(proxySrv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+proxySrv.URL[len("http"):]+"/websocket", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	start := time.Now()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, readErr := conn.ReadMessage()
	elapsed := time.Since(start)
	if ne, ok := readErr.(interface{ Timeout() bool }); ok && ne.Timeout() {
		t.Fatal("client still connected 5s after the backend went silent (websocket_idle 1s)")
	}
	// The deadline is armed slightly before this clock starts, so allow some
	// slack; the bound still tells 1s from an immediate close.
	if elapsed < 500*time.Millisecond {
		t.Errorf("connection closed after %v, long before the configured 1s idle", elapsed)
	}

	deadline := time.Now().Add(2 * time.Second)
	for cb.GetErrorCount(network, "node-1", "rpc") != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("circuit breaker errors = %d, want 1 for an idle backend", cb.GetErrorCount(network, "node-1", "rpc"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
