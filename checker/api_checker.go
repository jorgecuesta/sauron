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
		store:  store,
		cache:  cache,
		client: newCheckerHTTPClient(),
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
		recordCheckError(c.logger, node.Network, node.Name, "api", "request_creation", err)
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.client.Do(req)
	latency := time.Since(start)

	if err != nil {
		recordCheckError(c.logger, node.Network, node.Name, "api", "network", err)
		metrics.NodeAvailable.WithLabelValues(node.Network, node.Name, "api").Set(0)
		return fmt.Errorf("failed to fetch block: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		recordCheckError(c.logger, node.Network, node.Name, "api", "http_status", fmt.Errorf("status code %d", resp.StatusCode))
		metrics.NodeAvailable.WithLabelValues(node.Network, node.Name, "api").Set(0)
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		recordCheckError(c.logger, node.Network, node.Name, "api", "read_body", err)
		return fmt.Errorf("failed to read response: %w", err)
	}

	var apiResp APIBlockResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		recordCheckError(c.logger, node.Network, node.Name, "api", "json_parse", err)
		return fmt.Errorf("failed to parse JSON: %w", err)
	}

	// Try sdk_block.header.height first, fallback to block.header.height
	heightStr := apiResp.SDKBlock.Header.Height
	if heightStr == "" {
		heightStr = apiResp.Block.Header.Height
	}

	if heightStr == "" {
		recordCheckError(c.logger, node.Network, node.Name, "api", "height_missing", fmt.Errorf("height not found in response"))
		return fmt.Errorf("height not found in response")
	}

	height, err := strconv.ParseInt(heightStr, 10, 64)
	if err != nil {
		recordCheckError(c.logger, node.Network, node.Name, "api", "height_parse", err)
		return fmt.Errorf("failed to parse height '%s': %w", heightStr, err)
	}

	recordCheckSuccess(c.store, c.cache, c.logger, ctx, node.Network, node.Name, "api", height, latency)
	return nil
}

// Close shuts down the HTTP client and closes idle connections
func (c *APIChecker) Close() {
	if transport, ok := c.client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}
