package checker

import (
	"context"
	"net/http"
	"time"

	"sauron/metrics"
	"sauron/storage"

	"go.uber.org/zap"
)

// newCheckerHTTPClient creates a shared HTTP client with the standard
// connection-pool settings for internal checkers (API and RPC).
func newCheckerHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        HTTPMaxIdleConns,
			MaxIdleConnsPerHost: HTTPMaxIdleConnsPerHost,
			MaxConnsPerHost:     HTTPMaxConnsPerHost,
			IdleConnTimeout:     HTTPIdleConnTimeout,
		},
	}
}

// recordCheckError increments the error counter and logs at Warn level.
// network and node identify the source; endpointType is "api", "rpc", or "grpc";
// errorType is a short label (e.g. "network", "http_status").
func recordCheckError(logger *zap.Logger, network, node, endpointType, errorType string, err error) {
	metrics.HeightCheckErrors.WithLabelValues(network, node, endpointType, errorType).Inc()
	logger.Warn("Height check failed",
		zap.String("node", node),
		zap.String("network", network),
		zap.String("endpoint_type", endpointType),
		zap.String("error_type", errorType),
		zap.Error(err),
	)
}

// recordCheckSuccess updates the height store, optional cache, and all
// standard Prometheus metrics after a successful check.
func recordCheckSuccess(
	store *storage.HeightStore,
	cache *storage.Cache,
	logger *zap.Logger,
	ctx context.Context,
	network, node, endpointType string,
	height int64,
	latency time.Duration,
) {
	store.Update(network, node, endpointType, height, latency, "internal")

	if cache.IsEnabled() {
		cache.SetHeight(ctx, network, node, endpointType, height, 30*time.Second)
		cache.SetLatency(ctx, network, node, endpointType, latency, 30*time.Second)
	}

	metrics.NodeHeight.WithLabelValues(network, node, endpointType, "internal").Set(float64(height))
	metrics.NodeLatency.WithLabelValues(network, node, endpointType).Observe(latency.Seconds())
	metrics.NodeAvailable.WithLabelValues(network, node, endpointType).Set(1)
	metrics.HeightCheckDuration.WithLabelValues(network, node, endpointType).Observe(latency.Seconds())

	logger.Debug("Height check successful",
		zap.String("node", node),
		zap.String("network", network),
		zap.String("endpoint_type", endpointType),
		zap.Int64("height", height),
		zap.Duration("latency", latency),
	)
}
