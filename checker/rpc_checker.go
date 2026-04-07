package checker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"sauron/config"
	"sauron/internal/urlutil"
	"sauron/metrics"
	"sauron/storage"

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
		store:  store,
		cache:  cache,
		client: newCheckerHTTPClient(),
		logger: logger,
	}
}

// CheckNode checks the height of a single node via RPC
func (c *RPCChecker) CheckNode(ctx context.Context, node config.Node) error {
	if node.RPC == "" {
		return fmt.Errorf("node %s has no RPC endpoint configured", node.Name)
	}

	// Build URL (add https:// if missing, /status endpoint)
	url := urlutil.NormalizeURL(node.RPC) + "/status"

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		recordCheckError(c.logger, node.Network, node.Name, "rpc", "request_creation", err)
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.client.Do(req)
	latency := time.Since(start)

	if err != nil {
		recordCheckError(c.logger, node.Network, node.Name, "rpc", "network", err)
		metrics.NodeAvailable.WithLabelValues(node.Network, node.Name, "rpc").Set(0)
		return fmt.Errorf("failed to fetch status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		recordCheckError(c.logger, node.Network, node.Name, "rpc", "http_status", fmt.Errorf("status code %d", resp.StatusCode))
		metrics.NodeAvailable.WithLabelValues(node.Network, node.Name, "rpc").Set(0)
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		recordCheckError(c.logger, node.Network, node.Name, "rpc", "read_body", err)
		return fmt.Errorf("failed to read response: %w", err)
	}

	var rpcResp RPCStatusResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		recordCheckError(c.logger, node.Network, node.Name, "rpc", "json_parse", err)
		return fmt.Errorf("failed to parse JSON: %w", err)
	}

	// Parse height (it's a string in the response)
	heightStr := rpcResp.Result.SyncInfo.LatestBlockHeight
	height, err := strconv.ParseInt(heightStr, 10, 64)
	if err != nil {
		recordCheckError(c.logger, node.Network, node.Name, "rpc", "height_parse", err)
		return fmt.Errorf("failed to parse height '%s': %w", heightStr, err)
	}

	recordCheckSuccess(c.store, c.cache, c.logger, ctx, node.Network, node.Name, "rpc", height, latency)

	// Check WebSocket connectivity
	wsAvailable := CheckWebSocket(ctx, node.RPC, c.logger)
	c.store.UpdateWebSocketAvailability(node.Network, node.Name, "rpc", wsAvailable)

	if wsAvailable {
		metrics.NodeWebSocketAvailable.WithLabelValues(node.Network, node.Name, "rpc").Set(1)
	} else {
		metrics.NodeWebSocketAvailable.WithLabelValues(node.Network, node.Name, "rpc").Set(0)
		metrics.WebSocketCheckErrors.WithLabelValues(node.Network, node.Name, "rpc", "connectivity_failed").Inc()
	}

	c.logger.Debug("RPC height check complete",
		zap.String("node", node.Name),
		zap.String("network", node.Network),
		zap.Bool("websocket_available", wsAvailable),
	)

	return nil
}

// Close shuts down the HTTP client and closes idle connections
func (c *RPCChecker) Close() {
	if transport, ok := c.client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}
