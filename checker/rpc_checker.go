package checker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sauron/config"
	"sauron/metrics"
	"sauron/storage"

	"github.com/gorilla/websocket"

	"go.uber.org/zap"
)

// RPCChecker checks node heights via Tendermint RPC /status endpoint
// The Eye gazing upon the RPC realm
type RPCChecker struct {
	store  *storage.HeightStore
	cache  *storage.Cache
	client *http.Client
	logger *zap.Logger
}

// RPCStatusResponse represents the Tendermint RPC /status response
type RPCStatusResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Result  struct {
		SyncInfo struct {
			LatestBlockHeight string `json:"latest_block_height"`
		} `json:"sync_info"`
	} `json:"result"`
}

// NewRPCChecker creates a new RPC checker
func NewRPCChecker(store *storage.HeightStore, cache *storage.Cache, logger *zap.Logger) *RPCChecker {
	return &RPCChecker{
		store: store,
		cache: cache,
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        HTTPMaxIdleConns,
				MaxIdleConnsPerHost: HTTPMaxIdleConnsPerHost,
				MaxConnsPerHost:     HTTPMaxConnsPerHost,
				IdleConnTimeout:     HTTPIdleConnTimeout,
			},
		},
		logger: logger,
	}
}

// CheckNode checks the height of a single node via RPC
func (c *RPCChecker) CheckNode(ctx context.Context, node config.Node) error {
	if node.RPC == "" {
		return fmt.Errorf("node %s has no RPC endpoint configured", node.Name)
	}

	// Build URL (add https:// if missing, /status endpoint)
	url := node.RPC
	if len(url) > 0 && url[len(url)-1] == '/' {
		url = url[:len(url)-1]
	}
	if len(url) > 0 && url[0] != 'h' {
		url = "https://" + url
	}
	url += "/status"

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		c.recordError(node, "request_creation", err)
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.client.Do(req)
	latency := time.Since(start)

	if err != nil {
		c.recordError(node, "network", err)
		metrics.NodeAvailable.WithLabelValues(node.Network, node.Name, "rpc").Set(0)
		return fmt.Errorf("failed to fetch status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		c.recordError(node, "http_status", fmt.Errorf("status code %d", resp.StatusCode))
		metrics.NodeAvailable.WithLabelValues(node.Network, node.Name, "rpc").Set(0)
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.recordError(node, "read_body", err)
		return fmt.Errorf("failed to read response: %w", err)
	}

	var rpcResp RPCStatusResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		c.recordError(node, "json_parse", err)
		return fmt.Errorf("failed to parse JSON: %w", err)
	}

	// Parse height (it's a string in the response)
	heightStr := rpcResp.Result.SyncInfo.LatestBlockHeight
	height, err := strconv.ParseInt(heightStr, 10, 64)
	if err != nil {
		c.recordError(node, "height_parse", err)
		return fmt.Errorf("failed to parse height '%s': %w", heightStr, err)
	}

	// Update storage
	c.store.Update(node.Network, node.Name, "rpc", height, latency, "internal")

	// Check WebSocket connectivity
	wsAvailable := c.CheckWebSocketConnectivity(ctx, node)
	c.store.UpdateWebSocketAvailability(node.Network, node.Name, "rpc", wsAvailable)

	// Update WebSocket availability metric
	if wsAvailable {
		metrics.NodeWebSocketAvailable.WithLabelValues(node.Network, node.Name, "rpc").Set(1)
	} else {
		metrics.NodeWebSocketAvailable.WithLabelValues(node.Network, node.Name, "rpc").Set(0)
		metrics.WebSocketCheckErrors.WithLabelValues(node.Network, node.Name, "rpc", "connectivity_failed").Inc()
	}

	// Update cache if enabled
	if c.cache.IsEnabled() {
		c.cache.SetHeight(ctx, node.Network, node.Name, "rpc", height, 30*time.Second)
		c.cache.SetLatency(ctx, node.Network, node.Name, "rpc", latency, 30*time.Second)
	}

	// Update metrics
	metrics.NodeHeight.WithLabelValues(node.Network, node.Name, "rpc", "internal").Set(float64(height))
	metrics.NodeLatency.WithLabelValues(node.Network, node.Name, "rpc").Observe(latency.Seconds())
	metrics.NodeAvailable.WithLabelValues(node.Network, node.Name, "rpc").Set(1)
	metrics.HeightCheckDuration.WithLabelValues(node.Network, node.Name, "rpc").Observe(latency.Seconds())

	c.logger.Debug("RPC height check successful",
		zap.String("node", node.Name),
		zap.String("network", node.Network),
		zap.Int64("height", height),
		zap.Duration("latency", latency),
		zap.Bool("websocket_available", wsAvailable),
	)

	return nil
}

func (c *RPCChecker) recordError(node config.Node, errorType string, err error) {
	metrics.HeightCheckErrors.WithLabelValues(node.Network, node.Name, "rpc", errorType).Inc()
	c.logger.Warn("RPC height check failed",
		zap.String("node", node.Name),
		zap.String("network", node.Network),
		zap.String("error_type", errorType),
		zap.Error(err),
	)
}

// Close shuts down the HTTP client and closes idle connections
func (c *RPCChecker) Close() {
	if transport, ok := c.client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

// CheckWebSocketConnectivity tests if a node's WebSocket endpoint is working
// Returns true if WebSocket is available and working
func (c *RPCChecker) CheckWebSocketConnectivity(ctx context.Context, node config.Node) bool {
	if node.RPC == "" {
		return false
	}

	// Build WebSocket URL
	wsURL := node.RPC
	if len(wsURL) > 0 && wsURL[len(wsURL)-1] == '/' {
		wsURL = wsURL[:len(wsURL)-1]
	}

	// Convert http(s):// to ws(s)://
	if strings.HasPrefix(wsURL, "http://") {
		wsURL = "ws://" + wsURL[7:]
	} else if strings.HasPrefix(wsURL, "https://") {
		wsURL = "wss://" + wsURL[8:]
	} else if len(wsURL) > 0 && wsURL[0] != 'w' {
		// Assume https if no protocol specified
		wsURL = "wss://" + wsURL
	}
	wsURL += "/websocket"

	// Create isolated WebSocket dialer with timeout (avoid race on DefaultDialer)
	dialer := &websocket.Dialer{
		HandshakeTimeout: 3 * time.Second,
		Proxy:            websocket.DefaultDialer.Proxy,
	}

	// Connect to WebSocket
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		c.logger.Debug("WebSocket connection failed",
			zap.String("node", node.Name),
			zap.String("network", node.Network),
			zap.String("url", wsURL),
			zap.Error(err),
		)
		return false
	}
	defer func() { _ = conn.Close() }()

	// Set read deadline for response
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		c.logger.Debug("Failed to set read deadline",
			zap.String("node", node.Name),
			zap.String("network", node.Network),
			zap.Error(err),
		)
		return false
	}

	// Send a simple subscription test
	subscribeMsg := []byte(`{"jsonrpc":"2.0","method":"subscribe","id":1,"params":{"query":"tm.event='NewBlock'"}}`)
	if err := conn.WriteMessage(websocket.TextMessage, subscribeMsg); err != nil {
		c.logger.Debug("WebSocket write failed",
			zap.String("node", node.Name),
			zap.String("network", node.Network),
			zap.Error(err),
		)
		return false
	}

	// Try to read response
	_, _, err = conn.ReadMessage()
	if err != nil {
		c.logger.Debug("WebSocket read failed",
			zap.String("node", node.Name),
			zap.String("network", node.Network),
			zap.Error(err),
		)
		return false
	}

	// Cleanup: unsubscribe and close gracefully
	unsubscribeMsg := []byte(`{"jsonrpc":"2.0","method":"unsubscribe","id":2,"params":{"query":"tm.event='NewBlock'"}}`)
	_ = conn.WriteMessage(websocket.TextMessage, unsubscribeMsg)

	// Send close frame and wait for server response
	closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
	if err := conn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(time.Second)); err != nil {
		c.logger.Debug("Failed to send close message", zap.Error(err))
	}

	// Wait briefly for server close response before defer closes connection
	time.Sleep(100 * time.Millisecond)

	c.logger.Debug("WebSocket check successful",
		zap.String("node", node.Name),
		zap.String("network", node.Network),
		zap.String("url", wsURL),
	)

	return true
}
