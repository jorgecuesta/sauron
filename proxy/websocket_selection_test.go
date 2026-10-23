package proxy

import (
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

// TestHTTPProxy_WebSocketRoutedOnlyToCapableNodes (#13): with one node that
// supports WebSocket and one that does not, at the same height, every upgrade
// must reach the capable node, while plain requests still use both.
func TestHTTPProxy_WebSocketRoutedOnlyToCapableNodes(t *testing.T) {
	t.Parallel()

	const network = "ws-select-net"
	wsBackend := wsEchoBackend(t, false)
	plainHits := make(chan struct{}, 64)
	plainBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(plainBackend.Close)

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
internals:
  - name: node-plain
    network: ` + network + `
    endpoints:
      rpc: "` + plainBackend.URL + `"
  - name: node-ws
    network: ` + network + `
    endpoints:
      rpc: "` + wsBackend.URL + `"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)
	heightStore := storage.NewHeightStore()
	heightStore.Update(network, "node-plain", "rpc", 100, time.Millisecond, "internal")
	heightStore.Update(network, "node-ws", "rpc", 100, time.Millisecond, "internal")
	heightStore.UpdateWebSocketAvailability(network, "node-plain", "rpc", false)
	heightStore.UpdateWebSocketAvailability(network, "node-ws", "rpc", true)
	healthStore := storage.NewHealthStore()
	healthStore.SetHealthy(network, "node-plain", "rpc")
	healthStore.SetHealthy(network, "node-ws", "rpc")
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
	sel.SetHealthStore(healthStore)
	p := NewHTTPProxy(sel, cfgLoader, endpointStore, zap.NewNop(), "rpc", network, nil)

	proxySrv := httptest.NewServer(p)
	t.Cleanup(proxySrv.Close)
	wsURL := "ws" + proxySrv.URL[len("http"):] + "/websocket"

	const upgrades = 6
	for i := 0; i < upgrades; i++ {
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			t.Errorf("upgrade %d failed: HTTP %d, %v", i, status, err)
			continue
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
			t.Errorf("upgrade %d: write: %v", i, err)
		} else if _, msg, err := conn.ReadMessage(); err != nil || string(msg) != "ping" {
			t.Errorf("upgrade %d: echo = %q, %v", i, msg, err)
		}
		_ = conn.Close()
	}
	if n := len(plainHits); n != 0 {
		t.Errorf("node without WebSocket received %d upgrades, want 0", n)
	}

	// Plain requests are unaffected: they keep round-robin over both nodes.
	for i := 0; i < 4; i++ {
		resp, err := http.Get(proxySrv.URL + "/status")
		if err != nil {
			t.Fatalf("plain request %d: %v", i, err)
		}
		_ = resp.Body.Close()
	}
	if n := len(plainHits); n != 2 {
		t.Errorf("node-plain served %d of 4 plain requests, want 2 (round-robin over both)", n)
	}
}

// TestHTTPProxy_WebSocketNoCapableNode: when no node supports WebSocket, an
// upgrade is answered 503 without reaching any backend, and plain requests
// still work.
func TestHTTPProxy_WebSocketNoCapableNode(t *testing.T) {
	t.Parallel()

	const network = "ws-none-net"
	hits := make(chan struct{}, 16)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

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
internals:
  - name: node-1
    network: ` + network + `
    endpoints:
      rpc: "` + backend.URL + `"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)
	heightStore := storage.NewHeightStore()
	heightStore.Update(network, "node-1", "rpc", 100, time.Millisecond, "internal")
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
	p := NewHTTPProxy(sel, cfgLoader, endpointStore, zap.NewNop(), "rpc", network, nil)
	proxySrv := httptest.NewServer(p)
	t.Cleanup(proxySrv.Close)

	_, resp, err := websocket.DefaultDialer.Dial("ws"+proxySrv.URL[len("http"):]+"/websocket", nil)
	if err == nil {
		t.Fatal("upgrade succeeded with no WebSocket-capable node")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("upgrade response = %v, want 503", resp)
	}
	if n := len(hits); n != 0 {
		t.Errorf("backend received %d requests during the refused upgrade, want 0", n)
	}

	plain, err := http.Get(proxySrv.URL + "/status")
	if err != nil {
		t.Fatalf("plain request: %v", err)
	}
	_ = plain.Body.Close()
	if plain.StatusCode != http.StatusOK || len(hits) != 1 {
		t.Errorf("plain request: status %d, backend hits %d; want 200 and 1", plain.StatusCode, len(hits))
	}
}
