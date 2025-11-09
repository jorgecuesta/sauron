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

// APIChecker checks node heights via CosmosSDK REST API
// The Eye gazing upon the API realm
type APIChecker struct {
	store  *storage.HeightStore
	cache  *storage.Cache
	client *http.Client
	logger *zap.Logger
}

// APIBlockResponse represents the CosmosSDK /cosmos/base/tendermint/v1beta1/blocks/latest response
type APIBlockResponse struct {
	Block struct {
		Header struct {
			Height string `json:"height"`
		} `json:"header"`
	} `json:"block"`
	SDKBlock struct {
		Header struct {
			Height string `json:"height"`
		} `json:"header"`
	} `json:"sdk_block"`
}

// NewAPIChecker creates a new API checker
func NewAPIChecker(store *storage.HeightStore, cache *storage.Cache, logger *zap.Logger) *APIChecker {
	return &APIChecker{
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

// CheckNode checks the height of a single node via REST API
func (c *APIChecker) CheckNode(ctx context.Context, node config.Node) error {
	if node.API == "" {
		return fmt.Errorf("node %s has no API endpoint configured", node.Name)
	}

	// Build URL
	url := node.API
	if len(url) > 0 && url[len(url)-1] == '/' {
		url = url[:len(url)-1]
	}
	if len(url) > 0 && url[0] != 'h' {
		url = "https://" + url
	}
	url += "/cosmos/base/tendermint/v1beta1/blocks/latest"

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
		metrics.NodeAvailable.WithLabelValues(node.Network, node.Name, "api").Set(0)
		return fmt.Errorf("failed to fetch block: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		c.recordError(node, "http_status", fmt.Errorf("status code %d", resp.StatusCode))
		metrics.NodeAvailable.WithLabelValues(node.Network, node.Name, "api").Set(0)
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.recordError(node, "read_body", err)
		return fmt.Errorf("failed to read response: %w", err)
	}

	var apiResp APIBlockResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		c.recordError(node, "json_parse", err)
		return fmt.Errorf("failed to parse JSON: %w", err)
	}

	// Try sdk_block.header.height first, fallback to block.header.height
	heightStr := apiResp.SDKBlock.Header.Height
	if heightStr == "" {
		heightStr = apiResp.Block.Header.Height
	}

	if heightStr == "" {
		c.recordError(node, "height_missing", fmt.Errorf("height not found in response"))
		return fmt.Errorf("height not found in response")
	}

	height, err := strconv.ParseInt(heightStr, 10, 64)
	if err != nil {
		c.recordError(node, "height_parse", err)
		return fmt.Errorf("failed to parse height '%s': %w", heightStr, err)
	}

	// Update storage
	c.store.Update(node.Network, node.Name, "api", height, latency, "internal")

	// Update cache if enabled
	if c.cache.IsEnabled() {
		c.cache.SetHeight(ctx, node.Network, node.Name, "api", height, 30*time.Second)
		c.cache.SetLatency(ctx, node.Network, node.Name, "api", latency, 30*time.Second)
	}

	// Update metrics
	metrics.NodeHeight.WithLabelValues(node.Network, node.Name, "api", "internal").Set(float64(height))
	metrics.NodeLatency.WithLabelValues(node.Network, node.Name, "api").Observe(latency.Seconds())
	metrics.NodeAvailable.WithLabelValues(node.Network, node.Name, "api").Set(1)
	metrics.HeightCheckDuration.WithLabelValues(node.Network, node.Name, "api").Observe(latency.Seconds())

	c.logger.Debug("API height check successful",
		zap.String("node", node.Name),
		zap.String("network", node.Network),
		zap.Int64("height", height),
		zap.Duration("latency", latency),
	)

	return nil
}

func (c *APIChecker) recordError(node config.Node, errorType string, err error) {
	metrics.HeightCheckErrors.WithLabelValues(node.Network, node.Name, "api", errorType).Inc()
	c.logger.Warn("API height check failed",
		zap.String("node", node.Name),
		zap.String("network", node.Network),
		zap.String("error_type", errorType),
		zap.Error(err),
	)
}

// Close shuts down the HTTP client and closes idle connections
func (c *APIChecker) Close() {
	if transport, ok := c.client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}
