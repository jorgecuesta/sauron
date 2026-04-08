package checker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"sauron/config"
	"sauron/internal/grpcutil"
	"sauron/internal/urlutil"
	"sauron/metrics"
	"sauron/storage"

	tmservice "cosmossdk.io/api/cosmos/base/tendermint/v1beta1"
	"github.com/puzpuzpuz/xsync/v4"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// ExternalChecker queries other Sauron deployments (the Palantíri network)
// Peering into distant towers through the seeing-stones
type ExternalChecker struct {
	store           *storage.HeightStore
	endpointStore   *storage.ExternalEndpointStore
	configLoader    *config.Loader
	client          *http.Client
	logger          *zap.Logger
	grpcConnections *xsync.Map[string, *grpc.ClientConn] // url -> connection pool for external gRPC endpoints
}

// ExternalStatusResponse represents the response from another Sauron's status API
// Contains the max height and advertised connection endpoints
type ExternalStatusResponse struct {
	Height       int64  `json:"height"`                  // Maximum height reported by external ring
	API          string `json:"api,omitempty"`           // External API endpoint URL (if advertised)
	RPC          string `json:"rpc,omitempty"`           // External RPC endpoint URL (if advertised)
	GRPC         string `json:"grpc,omitempty"`          // External gRPC endpoint URL (if advertised)
	GRPCInsecure bool   `json:"grpc_insecure,omitempty"` // Whether advertised gRPC endpoint uses insecure (no TLS)
}

// NewExternalChecker creates a new external checker
func NewExternalChecker(store *storage.HeightStore, endpointStore *storage.ExternalEndpointStore, configLoader *config.Loader, logger *zap.Logger) *ExternalChecker {
	return &ExternalChecker{
		store:         store,
		endpointStore: endpointStore,
		configLoader:  configLoader,
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        ExternalHTTPMaxIdleConns,
				MaxIdleConnsPerHost: ExternalHTTPMaxIdleConnsPerHost,
				MaxConnsPerHost:     HTTPMaxConnsPerHost,
				IdleConnTimeout:     HTTPIdleConnTimeout,
			},
		},
		logger:          logger,
		grpcConnections: xsync.NewMap[string, *grpc.ClientConn](),
	}
}

// CheckExternal queries an external Sauron ring for a specific network
func (c *ExternalChecker) CheckExternal(ctx context.Context, external config.External, network string) error {
	if len(external.Rings) == 0 {
		return fmt.Errorf("external %s has no rings configured", external.Name)
	}

	// Query each ring URL
	for _, ringURL := range external.Rings {
		if err := c.queryRing(ctx, external, ringURL, network); err != nil {
			c.logger.Warn("Failed to query external ring",
				zap.String("external", external.Name),
				zap.String("ring", ringURL),
				zap.String("network", network),
				zap.Error(err),
			)
			continue // Try next ring
		}
	}

	return nil
}

func (c *ExternalChecker) queryRing(ctx context.Context, external config.External, ringURL, network string) error {
	// Build URL: {ring}/{network}/status
	url := fmt.Sprintf("%s/%s/status", urlutil.TrimTrailingSlash(ringURL), network)

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		c.recordError(external.Name, ringURL, "request_creation", err)
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Add Bearer token if configured (non-empty)
	if external.Token != "" {
		req.Header.Set("Authorization", "Bearer "+external.Token)
	}

	resp, err := c.client.Do(req)
	latency := time.Since(start)

	if err != nil {
		c.recordError(external.Name, ringURL, "network", err)
		metrics.ExternalRingAvailable.WithLabelValues(external.Name, ringURL).Set(0)
		return fmt.Errorf("failed to fetch status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		c.recordError(external.Name, ringURL, "http_status", fmt.Errorf("status code %d", resp.StatusCode))
		metrics.ExternalRingAvailable.WithLabelValues(external.Name, ringURL).Set(0)
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.recordError(external.Name, ringURL, "read_body", err)
		return fmt.Errorf("failed to read response: %w", err)
	}

	var status ExternalStatusResponse
	if err := json.Unmarshal(body, &status); err != nil {
		c.recordError(external.Name, ringURL, "json_parse", err)
		return fmt.Errorf("failed to parse JSON: %w", err)
	}

	// Validate we got a height
	if status.Height == 0 {
		c.recordError(external.Name, ringURL, "zero_height", fmt.Errorf("external ring returned zero height"))
		return fmt.Errorf("external ring returned zero height")
	}

	// Store advertised endpoints in endpoint store
	// This makes them visible but not validated yet
	// NOTE: We do NOT update the HeightStore here - external endpoints are only tracked
	// in the ExternalEndpointStore. The selector will add them to the candidate pool
	// with the "ext:{url}" prefix when needed.
	advertisedTypes := []string{}
	if status.API != "" {
		c.endpointStore.StoreAdvertised(external.Name, ringURL, network, "api", status.API)
		metrics.NodeHeight.WithLabelValues(network, external.Name, "api", "external").Set(float64(status.Height))
		advertisedTypes = append(advertisedTypes, "api")

		// Validate endpoint (connectivity check only, insecure=false for HTTP)
		c.validateEndpoint(ctx, external.Name, ringURL, network, "api", status.API, status.Height, false)
	}
	if status.RPC != "" {
		c.endpointStore.StoreAdvertised(external.Name, ringURL, network, "rpc", status.RPC)
		metrics.NodeHeight.WithLabelValues(network, external.Name, "rpc", "external").Set(float64(status.Height))
		advertisedTypes = append(advertisedTypes, "rpc")

		// Validate endpoint (insecure=false for HTTP)
		c.validateEndpoint(ctx, external.Name, ringURL, network, "rpc", status.RPC, status.Height, false)
	}
	if status.GRPC != "" {
		c.endpointStore.StoreAdvertised(external.Name, ringURL, network, "grpc", status.GRPC)
		metrics.NodeHeight.WithLabelValues(network, external.Name, "grpc", "external").Set(float64(status.Height))
		advertisedTypes = append(advertisedTypes, "grpc")

		// Validate endpoint (pass grpc_insecure value)
		c.validateEndpoint(ctx, external.Name, ringURL, network, "grpc", status.GRPC, status.Height, status.GRPCInsecure)
	}

	// Update metrics
	metrics.ExternalRingLatency.WithLabelValues(external.Name, ringURL).Observe(latency.Seconds())
	metrics.ExternalRingAvailable.WithLabelValues(external.Name, ringURL).Set(1)

	c.logger.Debug("External ring check successful",
		zap.String("external", external.Name),
		zap.String("ring", ringURL),
		zap.String("network", network),
		zap.Int64("height", status.Height),
		zap.Strings("advertised_types", advertisedTypes),
		zap.Duration("latency", latency),
	)

	return nil
}

func (c *ExternalChecker) recordError(externalName, ringURL, errorType string, err error) {
	metrics.ExternalRingErrors.WithLabelValues(externalName, ringURL, errorType).Inc()
	c.logger.Warn("External ring check failed",
		zap.String("external", externalName),
		zap.String("ring", ringURL),
		zap.String("error_type", errorType),
		zap.Error(err),
	)
}

// validateEndpoint performs a connectivity check on an advertised endpoint
// Verifies the endpoint is reachable and functional
// useInsecure parameter is only used for gRPC endpoints to determine TLS settings
func (c *ExternalChecker) validateEndpoint(ctx context.Context, externalName, ringURL, network, endpointType, url string, height int64, useInsecure bool) {
	start := time.Now()

	var err error
	var latency time.Duration

	switch endpointType {
	case "api", "rpc":
		// For HTTP endpoints, do a simple GET request to check connectivity
		latency, err = c.validateHTTPEndpoint(ctx, url)
	case "grpc":
		// For gRPC endpoints, perform actual validation with GetLatestBlock call
		latency, err = c.validateGRPCEndpoint(ctx, url, useInsecure)
	}

	if err != nil {
		c.endpointStore.MarkValidationFailed(externalName, ringURL, network, endpointType, url)
		c.logger.Warn("External endpoint validation failed",
			zap.String("external", externalName),
			zap.String("ring", ringURL),
			zap.String("network", network),
			zap.String("type", endpointType),
			zap.String("url", url),
			zap.Error(err),
		)
		return
	}

	// Mark as validated with the advertised height and measured latency
	c.endpointStore.MarkValidated(externalName, ringURL, network, endpointType, url, height, latency)

	// For RPC endpoints, also check WebSocket connectivity
	if endpointType == "rpc" {
		wsAvailable := CheckWebSocket(ctx, url, c.logger)
		c.endpointStore.UpdateWebSocketAvailability(externalName, ringURL, network, endpointType, url, wsAvailable)

		// Update WebSocket availability metric
		if wsAvailable {
			metrics.NodeWebSocketAvailable.WithLabelValues(network, externalName, "rpc").Set(1)
		} else {
			metrics.NodeWebSocketAvailable.WithLabelValues(network, externalName, "rpc").Set(0)
			metrics.WebSocketCheckErrors.WithLabelValues(network, externalName, "rpc", "connectivity_failed").Inc()
		}

		c.logger.Debug("External endpoint validated",
			zap.String("external", externalName),
			zap.String("ring", ringURL),
			zap.String("network", network),
			zap.String("type", endpointType),
			zap.String("url", url),
			zap.Int64("height", height),
			zap.Duration("validation_time", time.Since(start)),
			zap.Bool("websocket_available", wsAvailable),
		)
	} else {
		c.logger.Debug("External endpoint validated",
			zap.String("external", externalName),
			zap.String("ring", ringURL),
			zap.String("network", network),
			zap.String("type", endpointType),
			zap.String("url", url),
			zap.Int64("height", height),
			zap.Duration("validation_time", time.Since(start)),
		)
	}
}

// validateHTTPEndpoint checks if an HTTP endpoint is reachable
func (c *ExternalChecker) validateHTTPEndpoint(ctx context.Context, url string) (time.Duration, error) {
	// Simple connectivity check - try to connect to the endpoint
	// Use a HEAD request for minimal overhead
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.client.Do(req)
	latency := time.Since(start)

	if err != nil {
		return latency, fmt.Errorf("connectivity check failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Accept any non-5xx status code as "working"
	// 4xx might be normal for endpoints that require auth or specific paths
	if resp.StatusCode >= 500 {
		return latency, fmt.Errorf("server error: status code %d", resp.StatusCode)
	}

	return latency, nil
}

// validateGRPCEndpoint validates a gRPC endpoint by calling GetLatestBlock
func (c *ExternalChecker) validateGRPCEndpoint(ctx context.Context, url string, useInsecure bool) (time.Duration, error) {
	// Get or create gRPC connection
	conn, err := c.getGRPCConnection(url, useInsecure)
	if err != nil {
		return 0, fmt.Errorf("failed to create gRPC connection: %w", err)
	}

	// Create Tendermint service client
	client := tmservice.NewServiceClient(conn)

	// Call GetLatestBlock to verify the endpoint is working
	// Use the parent context which should have appropriate timeout
	start := time.Now()
	resp, err := client.GetLatestBlock(ctx, &tmservice.GetLatestBlockRequest{})
	latency := time.Since(start)

	if err != nil {
		return latency, fmt.Errorf("gRPC call failed: %w", err)
	}

	// Verify we got a valid response
	if resp.SdkBlock == nil || resp.SdkBlock.Header == nil {
		return latency, fmt.Errorf("invalid gRPC response: nil block or header")
	}

	return latency, nil
}

// getGRPCConnection returns an existing connection or creates a new one.
// LoadOrCompute guarantees the factory runs exactly once per key, eliminating
// the TOCTOU race where two goroutines both see a cache miss and both create connections.
func (c *ExternalChecker) getGRPCConnection(url string, useInsecure bool) (*grpc.ClientConn, error) {
	var createErr error

	conn, _ := c.grpcConnections.LoadOrCompute(url, func() (newConn *grpc.ClientConn, cancel bool) {
		newConn, createErr = c.createGRPCConnection(url, useInsecure)
		if createErr != nil {
			return nil, true // cancel the store; don't cache a nil value
		}
		return newConn, false
	})

	if createErr != nil {
		return nil, createErr
	}
	if conn == nil {
		return nil, fmt.Errorf("connection creation cancelled for url %s", url)
	}
	return conn, nil
}

// createGRPCConnection dials gRPC and warms up the connection.
func (c *ExternalChecker) createGRPCConnection(url string, useInsecure bool) (*grpc.ClientConn, error) {
	cfg := c.configLoader.Get()
	ka := keepalive.ClientParameters{
		Time:                cfg.GRPCKeepalive.Time,
		Timeout:             cfg.GRPCKeepalive.Timeout,
		PermitWithoutStream: cfg.GRPCKeepalive.PermitWithoutStream,
	}

	conn, err := grpcutil.NewConnection(url, useInsecure, ka,
		grpc.WithConnectParams(grpc.ConnectParams{
			MinConnectTimeout: 10 * time.Second,
		}),
	)
	if err != nil {
		return nil, err
	}

	// Warm up — best effort, non-blocking
	client := tmservice.NewServiceClient(conn)
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := client.GetLatestBlock(warmupCtx, &tmservice.GetLatestBlockRequest{}); err != nil {
		c.logger.Warn("gRPC connection warmup failed, will establish on first validation",
			zap.String("url", url),
			zap.Error(err),
		)
	} else {
		c.logger.Debug("gRPC connection established and warmed up",
			zap.String("url", url),
		)
	}

	return conn, nil
}

// RecoverFailedEndpoints attempts to re-validate failed endpoints
// Called periodically to check if failed endpoints have recovered
func (c *ExternalChecker) RecoverFailedEndpoints(ctx context.Context) {
	failed := c.endpointStore.GetFailedEndpoints()

	if len(failed) == 0 {
		return
	}

	c.logger.Debug("Checking failed endpoints for recovery",
		zap.Int("count", len(failed)),
	)

	for _, ep := range failed {
		// Attempt to re-validate the endpoint
		var err error
		var latency time.Duration

		switch ep.Type {
		case "api", "rpc":
			latency, err = c.validateHTTPEndpoint(ctx, ep.URL)
		case "grpc":
			// Default to TLS (false) for recovery - safer default
			// TODO: Store TLS preference in endpoint store for more accurate recovery
			latency, err = c.validateGRPCEndpoint(ctx, ep.URL, false)
		}

		if err != nil {
			// Still failing, keep it failed
			c.logger.Debug("Failed endpoint still not working",
				zap.String("external", ep.ExternalName),
				zap.String("network", ep.Network),
				zap.String("type", ep.Type),
				zap.String("url", ep.URL),
				zap.Error(err),
			)
			continue
		}

		// Endpoint has recovered! Mark it as validated and working again
		c.endpointStore.MarkValidated(ep.ExternalName, ep.RingURL, ep.Network, ep.Type, ep.URL, ep.Height, latency)

		// For RPC endpoints, also check WebSocket connectivity after recovery
		if ep.Type == "rpc" {
			wsAvailable := CheckWebSocket(ctx, ep.URL, c.logger)
			c.endpointStore.UpdateWebSocketAvailability(ep.ExternalName, ep.RingURL, ep.Network, ep.Type, ep.URL, wsAvailable)

			if wsAvailable {
				metrics.NodeWebSocketAvailable.WithLabelValues(ep.Network, ep.ExternalName, "rpc").Set(1)
			} else {
				metrics.NodeWebSocketAvailable.WithLabelValues(ep.Network, ep.ExternalName, "rpc").Set(0)
				metrics.WebSocketCheckErrors.WithLabelValues(ep.Network, ep.ExternalName, "rpc", "connectivity_failed").Inc()
			}
		}

		// Record recovery metric
		metrics.ExternalEndpointRecoveries.WithLabelValues(ep.Network, ep.Type, ep.ExternalName).Inc()

		c.logger.Info("Failed endpoint has recovered",
			zap.String("external", ep.ExternalName),
			zap.String("ring", ep.RingURL),
			zap.String("network", ep.Network),
			zap.String("type", ep.Type),
			zap.String("url", ep.URL),
			zap.Duration("latency", latency),
		)
	}
}

// UpdateEndpointMetrics updates aggregate endpoint metrics
func (c *ExternalChecker) UpdateEndpointMetrics() {
	c.endpointStore.UpdateAggregateMetrics()
}

// Close shuts down the HTTP client and closes idle connections
func (c *ExternalChecker) Close() {
	if transport, ok := c.client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}

	// Close all gRPC connections
	c.grpcConnections.Range(func(url string, conn *grpc.ClientConn) bool {
		if err := conn.Close(); err != nil {
			c.logger.Warn("Failed to close external gRPC connection",
				zap.String("url", url),
				zap.Error(err),
			)
		}
		return true // continue iteration
	})
	c.grpcConnections.Clear()
}
