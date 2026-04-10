package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

func TestIsWebSocketRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		connection string
		upgrade    string
		want       bool
	}{
		{"valid upgrade", "Upgrade", "websocket", true},
		{"lowercase", "upgrade", "websocket", true},
		{"mixed case", "Upgrade", "WebSocket", true},
		{"keep-alive,upgrade", "keep-alive, Upgrade", "websocket", true},
		{"no upgrade header", "", "websocket", false},
		{"no websocket value", "Upgrade", "", false},
		{"wrong upgrade value", "Upgrade", "h2c", false},
		{"empty headers", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest("GET", "/ws", nil)
			if tt.connection != "" {
				req.Header.Set("Connection", tt.connection)
			}
			if tt.upgrade != "" {
				req.Header.Set("Upgrade", tt.upgrade)
			}
			got := IsWebSocketRequest(req)
			if got != tt.want {
				t.Errorf("IsWebSocketRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProxyWebSocket_NonHijackableWriter(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")

	rr := httptest.NewRecorder()
	statusCode, err := ProxyWebSocket(rr, req, "http://127.0.0.1:8080", zap.NewNop())

	if err == nil {
		t.Fatal("expected error for non-hijackable writer")
	}
	if statusCode != 0 {
		t.Errorf("expected status 0 (backend never reached), got %d", statusCode)
	}

	var wsErr *WSProxyError
	if !errors.As(err, &wsErr) {
		t.Fatalf("expected *WSProxyError, got %T", err)
	}
	if wsErr.Category != "backend_connect_error" {
		t.Errorf("expected category backend_connect_error, got %q", wsErr.Category)
	}
}

func TestProxyWebSocket_InvalidURL(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest("GET", "/ws", nil)
	rr := httptest.NewRecorder()
	_, err := ProxyWebSocket(rr, req, "://invalid", zap.NewNop())

	if err == nil {
		t.Fatal("expected error for invalid URL")
	}

	var wsErr *WSProxyError
	if !errors.As(err, &wsErr) {
		t.Fatalf("expected *WSProxyError, got %T", err)
	}
	if wsErr.Category != "backend_connect_error" {
		t.Errorf("expected category backend_connect_error, got %q", wsErr.Category)
	}
}

func TestProxyWebSocket_BackendUnreachable(t *testing.T) {
	t.Parallel()

	type result struct {
		status int
		err    error
	}
	resultCh := make(chan result, 1)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, e := ProxyWebSocket(w, r, "http://127.0.0.1:1", zap.NewNop())
		resultCh <- result{s, e}
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")

	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}

	res := <-resultCh
	if res.err == nil {
		t.Fatal("expected error for unreachable backend")
	}
	if res.status != 502 {
		t.Errorf("expected status 502, got %d", res.status)
	}

	var wsErr *WSProxyError
	if !errors.As(res.err, &wsErr) {
		t.Fatalf("expected *WSProxyError, got %T", res.err)
	}
	if wsErr.Category != "backend_connect_error" {
		t.Errorf("expected category backend_connect_error, got %q", wsErr.Category)
	}
}

// TestProxyWebSocket_HappyPath starts a real WebSocket server, proxies through
// ProxyWebSocket, and verifies a message passes end-to-end.
func TestProxyWebSocket_HappyPath(t *testing.T) {
	t.Parallel()

	// Echo WebSocket server: reads one message, echoes it back.
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	echoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.WriteMessage(mt, msg)
	}))
	defer echoServer.Close()

	type result struct {
		status int
		err    error
	}
	resultCh := make(chan result, 1)

	// Proxy server: proxies WebSocket to the echo server.
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, e := ProxyWebSocket(w, r, echoServer.URL, zap.NewNop())
		resultCh <- result{s, e}
	}))
	defer proxyServer.Close()

	// Connect via gorilla/websocket client through the proxy.
	wsURL := "ws" + proxyServer.URL[len("http"):]
	conn, _, err := websocket.DefaultDialer.Dial(wsURL+"/ws", nil)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Send a message and verify echo.
	sentMsg := "hello sauron"
	if err := conn.WriteMessage(websocket.TextMessage, []byte(sentMsg)); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, gotMsg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(gotMsg) != sentMsg {
		t.Errorf("echo mismatch: got %q, want %q", string(gotMsg), sentMsg)
	}

	// Close and wait for proxy to finish.
	_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	_ = conn.Close()

	res := <-resultCh
	// Status should be 101 (Switching Protocols).
	if res.status != http.StatusSwitchingProtocols {
		t.Errorf("expected status 101, got %d", res.status)
	}
	// No error on clean close.
	if res.err != nil {
		t.Errorf("expected no error, got: %v", res.err)
	}
}
