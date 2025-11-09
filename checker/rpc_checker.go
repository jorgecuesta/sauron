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
