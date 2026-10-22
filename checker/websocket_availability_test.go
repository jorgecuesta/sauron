package checker

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"sauron/proxy"
	"sauron/selector"
	"sauron/storage"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// cosmosNodeServer fakes a Cosmos node's RPC port: /status for the height
// check and, when withWS is true, a /websocket endpoint that accepts a
// subscription the way CometBFT does.
func cosmosNodeServer(t *testing.T, withWS bool) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			_, _ = w.Write([]byte(`{"result":{"sync_info":{"latest_block_height":"976232"}}}`))
		case "/cosmos/base/tendermint/v1beta1/blocks/latest":
			_, _ = w.Write([]byte(`{"sdk_block":{"header":{"height":"976232"}}}`))
		case "/websocket":
			if !withWS {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCheckInternalNodes_WebSocketAvailability: an internal Cosmos node
// configured like production (only rest/rpc URLs, no explicit "websocket"
// endpoint) must have its WebSocket support checked through its RPC URL, and
// the result must land on the height-store entry the selector returns, which
// is what the HTTP proxy reads before allowing a WebSocket upgrade.
func TestCheckInternalNodes_WebSocketAvailability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		withWS bool
		want   bool
	}{
		{name: "node serves /websocket", withWS: true, want: true},
		{name: "node without websocket", withWS: false, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := cosmosNodeServer(t, tc.withWS)
			sched, heightStore, healthStore := testMultiChainSetup(t, srv.URL)

			sched.checkInternalNodes()
			drainTestPool(sched.pool)

			nodes := heightStore.GetByNetwork("pocket", "rpc")
			m, ok := nodes["seed-one"]
			if !ok {
				t.Fatalf("no height entry for seed-one; got %v", nodes)
			}
			if m.Height != 976232 {
				t.Errorf("height = %d, want 976232", m.Height)
			}
			if m.WebSocketAvailable != tc.want {
				t.Errorf("WebSocketAvailable = %v, want %v", m.WebSocketAvailable, tc.want)
			}
			if got := healthStore.IsHealthy("pocket", "seed-one", "websocket"); got != tc.want {
				t.Errorf("websocket health = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWebSocketThroughProxyAfterHealthCheck wires the scheduler and the HTTP
// proxy the way server.New does (shared height and health stores, selector
// with the health store) and proves the end-to-end path that regressed in
// 2.0.0-beta.2: after a health-check cycle, a WebSocket upgrade through the RPC
// proxy reaches the node instead of being rejected with 503.
func TestWebSocketThroughProxyAfterHealthCheck(t *testing.T) {
	t.Parallel()

	srv := cosmosNodeServer(t, true)
	sched, heightStore, healthStore := testMultiChainSetup(t, srv.URL)

	sched.checkInternalNodes()
	drainTestPool(sched.pool)

	sel := selector.NewSelector(heightStore, storage.NewExternalEndpointStore(zap.NewNop()), sched.configLoader, zap.NewNop())
	sel.SetHealthStore(healthStore)
	p := proxy.NewHTTPProxy(sel, sched.configLoader, storage.NewExternalEndpointStore(zap.NewNop()), zap.NewNop(), "rpc", "pocket", nil)
	proxySrv := httptest.NewServer(p)
	t.Cleanup(proxySrv.Close)

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+proxySrv.URL[len("http"):]+"/websocket", nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("WebSocket upgrade through the proxy failed: %v (HTTP %d)", err, status)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","method":"subscribe","id":1,"params":{"query":"tm.event='NewBlock'"}}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(msg) != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Errorf("reply = %s, want the node's subscription result", msg)
	}
}
