package checker

import (
	"context"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// CheckWebSocket tests WebSocket connectivity for a Tendermint RPC endpoint.
//
// httpURL must be an http:// or https:// URL (or bare host). The function:
//  1. Converts the URL scheme to ws:// or wss://
//  2. Appends /websocket
//  3. Dials with a 3-second handshake timeout
//  4. Sets a 3-second read deadline
//  5. Sends a tm.event='NewBlock' subscribe message
//  6. Reads the response
//  7. Sends unsubscribe + close frame
//
// Returns true if all steps succeed.
func CheckWebSocket(ctx context.Context, httpURL string, logger *zap.Logger) bool {
	if httpURL == "" {
		return false
	}

	// Trim trailing slash
	wsURL := httpURL
	if len(wsURL) > 0 && wsURL[len(wsURL)-1] == '/' {
		wsURL = wsURL[:len(wsURL)-1]
	}

	// Convert http(s):// → ws(s)://
	switch {
	case strings.HasPrefix(wsURL, "http://"):
		wsURL = "ws://" + wsURL[len("http://"):]
	case strings.HasPrefix(wsURL, "https://"):
		wsURL = "wss://" + wsURL[len("https://"):]
	case len(wsURL) > 0 && wsURL[0] != 'w':
		// No scheme — assume TLS
		wsURL = "wss://" + wsURL
	}
	wsURL += "/websocket"

	// Isolated dialer so we never race on websocket.DefaultDialer
	dialer := &websocket.Dialer{
		HandshakeTimeout: 3 * time.Second,
		Proxy:            websocket.DefaultDialer.Proxy,
	}

	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		logger.Debug("WebSocket connection failed",
			zap.String("url", wsURL),
			zap.Error(err),
		)
		return false
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		logger.Debug("Failed to set WebSocket read deadline",
			zap.String("url", wsURL),
			zap.Error(err),
		)
		return false
	}

	subscribeMsg := []byte(`{"jsonrpc":"2.0","method":"subscribe","id":1,"params":{"query":"tm.event='NewBlock'"}}`)
	if err := conn.WriteMessage(websocket.TextMessage, subscribeMsg); err != nil {
		logger.Debug("WebSocket write failed",
			zap.String("url", wsURL),
			zap.Error(err),
		)
		return false
	}

	if _, _, err = conn.ReadMessage(); err != nil {
		logger.Debug("WebSocket read failed",
			zap.String("url", wsURL),
			zap.Error(err),
		)
		return false
	}

	// Graceful teardown
	unsubscribeMsg := []byte(`{"jsonrpc":"2.0","method":"unsubscribe","id":2,"params":{"query":"tm.event='NewBlock'"}}`)
	_ = conn.WriteMessage(websocket.TextMessage, unsubscribeMsg)
	_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))

	logger.Debug("WebSocket check successful", zap.String("url", wsURL))
	return true
}
