package proxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"sauron/config"
	"sauron/metrics"
	"sauron/selector"
	"sauron/storage"

	"go.uber.org/zap"
)

// HTTPProxy handles HTTP/API and RPC proxying
// The gates through which the Ringwraiths pass
type HTTPProxy struct {
	selector      *selector.Selector
	configLoader  *config.Loader
	endpointStore *storage.ExternalEndpointStore
	transport     *http.Transport
	logger        *zap.Logger
	endpointType  string // "api" or "rpc"
	network       string // The network this proxy serves
}

// NewHTTPProxy creates a new HTTP proxy for a specific network
func NewHTTPProxy(
	selector *selector.Selector,
	configLoader *config.Loader,
	endpointStore *storage.ExternalEndpointStore,
	logger *zap.Logger,
	endpointType string,
	network string,
) *HTTPProxy {
	// Optimized transport for maximum throughput
	transport := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		MaxConnsPerHost:       0, // Unlimited
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second, // Will be updated from config
		TLSHandshakeTimeout:   10 * time.Second,
	}

	return &HTTPProxy{
		selector:      selector,
		configLoader:  configLoader,
		endpointStore: endpointStore,
		transport:     transport,
		logger:        logger,
		endpointType:  endpointType,
		network:       network,
	}
}

// ServeHTTP handles the proxy request
func (p *HTTPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Log every request for debugging
	p.logger.Info("Proxy request received",
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
		zap.String("type", p.endpointType),
	)

	// Update timeout from config
	cfg := p.configLoader.Get()
	p.transport.ResponseHeaderTimeout = cfg.Timeouts.Proxy

	// Use the network this proxy is configured for (no detection needed!)
	network := p.network

	// Select best node
	nodeMetrics, nodeName, decision := p.selector.GetBestNode(network, p.endpointType)
	if nodeMetrics == nil || nodeName == "" {
		p.logger.Warn("No available nodes for routing",
			zap.String("network", network),
			zap.String("type", p.endpointType),
		)
		http.Error(w, "No available nodes", http.StatusServiceUnavailable)
		return
	}

	// Get endpoint URL
	targetURL := p.selector.GetEndpointURL(nodeName, p.endpointType)
	if targetURL == "" {
		p.logger.Error("Failed to get endpoint URL",
			zap.String("node", nodeName),
			zap.String("type", p.endpointType),
		)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	p.logger.Info("Routing decision made",
		zap.String("network", network),
		zap.String("selected_node", nodeName),
		zap.String("target_url", targetURL),
		zap.String("path", r.URL.Path),
	)

	// Parse target URL
	target, err := url.Parse(targetURL)
	if err != nil {
		p.logger.Error("Failed to parse target URL",
			zap.String("url", targetURL),
			zap.Error(err),
		)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Create reverse proxy
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = p.transport

	// Customize the Director to properly forward path, headers, and query params
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		// CRITICAL: Set the Host header to the backend host, not the proxy host
		req.Host = target.Host
		// Log what we're sending to backend
		p.logger.Info("Outgoing request to backend",
			zap.String("method", req.Method),
			zap.String("url", req.URL.String()),
			zap.String("host", req.Host),
			zap.String("path", req.URL.Path),
			zap.String("raw_query", req.URL.RawQuery),
		)
	}

	// Add error handler to log proxy errors
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		p.logger.Error("Reverse proxy error",
			zap.Error(err),
			zap.String("path", r.URL.Path),
			zap.String("backend", target.Host),
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}

	// Wrap response writer to track status and size
	tracker := &responseTracker{ResponseWriter: w, statusCode: 200}

	// Proxy the request
	p.logger.Info("Proxying to backend",
		zap.String("backend_host", target.Host),
		zap.String("backend_scheme", target.Scheme),
		zap.String("request_path", r.URL.Path),
		zap.String("request_query", r.URL.RawQuery),
	)
	proxy.ServeHTTP(tracker, r)

	p.logger.Info("Backend response received",
		zap.Int("status_code", tracker.statusCode),
		zap.Int64("response_bytes", tracker.bytesWritten),
	)

	// Record metrics
	duration := time.Since(start)
	statusStr := strconv.Itoa(tracker.statusCode)

	metrics.ProxyRequestDuration.WithLabelValues(
		network,
		nodeName,
		p.endpointType,
		statusStr,
	).Observe(duration.Seconds())

	metrics.ProxyResponseSize.WithLabelValues(network, p.endpointType).Observe(float64(tracker.bytesWritten))
	metrics.NodeRequests.WithLabelValues(network, nodeName, p.endpointType, r.Method).Inc()

	if tracker.statusCode >= 400 {
		metrics.ProxyErrors.WithLabelValues(network, nodeName, p.endpointType, statusStr, "http_error").Inc()
	}

	// Track 5xx errors for external endpoints
	if tracker.statusCode >= 500 && p.endpointStore != nil {
		if p.endpointStore.TrackProxyError(network, p.endpointType, targetURL) {
			p.logger.Info("Tracked 5xx error for external endpoint",
				zap.String("url", targetURL),
				zap.String("network", network),
				zap.String("type", p.endpointType),
				zap.Int("status", tracker.statusCode),
			)
		}
	}

	p.logger.Debug("Request proxied",
		zap.String("network", network),
		zap.String("node", nodeName),
		zap.String("type", p.endpointType),
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
		zap.Int("status", tracker.statusCode),
		zap.Int64("bytes", tracker.bytesWritten),
		zap.Duration("duration", duration),
		zap.String("selection_reason", decision.Reason),
	)
}

// responseTracker tracks response status and size
type responseTracker struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int64
}

func (rt *responseTracker) WriteHeader(code int) {
	rt.statusCode = code
	rt.ResponseWriter.WriteHeader(code)
}

func (rt *responseTracker) Write(b []byte) (int, error) {
	n, err := rt.ResponseWriter.Write(b)
	rt.bytesWritten += int64(n)
	return n, err
}
