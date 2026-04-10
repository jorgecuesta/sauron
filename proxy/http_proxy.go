package proxy

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"sauron/config"
	"sauron/metrics"
	"sauron/selector"
	"sauron/storage"

	"go.uber.org/zap"
)

// contextKey is an unexported type for context keys in this package.
type contextKey int

const targetContextKey contextKey = iota

// singleJoiningSlash replicates the stdlib helper used by httputil.NewSingleHostReverseProxy.
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

// HTTPProxy handles HTTP/API and RPC proxying
// The gates through which the Ringwraiths pass
type HTTPProxy struct {
	selector       *selector.Selector
	configLoader   *config.MultiChainLoader
	endpointStore  *storage.ExternalEndpointStore
	transport      *http.Transport
	reverseProxy   *httputil.ReverseProxy
	logger         *zap.Logger
	endpointType   string // V2 protocol name: "rest", "rpc", "jsonrpc", "http", etc.
	network        string // The network this proxy serves
	proxyTimeoutNs atomic.Int64
}

// NewHTTPProxy creates a new HTTP proxy for a specific network
func NewHTTPProxy(
	selector *selector.Selector,
	configLoader *config.MultiChainLoader,
	endpointStore *storage.ExternalEndpointStore,
	logger *zap.Logger,
	endpointType string,
	network string,
) *HTTPProxy {
	// C1 FIX: Read the timeout once here; never mutate transport after construction.
	cfg := configLoader.Get()
	proxyTimeout := cfg.Timeouts.Proxy
	if proxyTimeout == 0 {
		proxyTimeout = 60 * time.Second
	}

	// Optimized transport for maximum throughput.
	// ResponseHeaderTimeout is set once, never mutated per-request.
	transport := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		MaxConnsPerHost:       0, // Unlimited
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: proxyTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
	}

	p := &HTTPProxy{
		selector:      selector,
		configLoader:  configLoader,
		endpointStore: endpointStore,
		transport:     transport,
		logger:        logger,
		endpointType:  endpointType,
		network:       network,
	}
	// Store timeout as nanoseconds for potential future hot-reload.
	p.proxyTimeoutNs.Store(int64(proxyTimeout))

	// H1 FIX: Build ONE shared ReverseProxy in the constructor.
	// The Director reads the target URL from request context so it is
	// safe to share across all concurrent requests.
	rp := &httputil.ReverseProxy{
		Transport: transport,
		Director: func(req *http.Request) {
			target, _ := req.Context().Value(targetContextKey).(*url.URL)
			if target == nil {
				return
			}

			// Replicate full httputil.NewSingleHostReverseProxy Director behavior:
			// 1. Set scheme and host.
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host

			// 2. Merge paths using singleJoiningSlash.
			req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
			if target.Path != "" && req.URL.RawPath != "" {
				req.URL.RawPath = singleJoiningSlash(target.RawPath, req.URL.RawPath)
			}

			// 3. Merge RawQuery — join with "&" when both non-empty.
			if target.RawQuery == "" || req.URL.RawQuery == "" {
				req.URL.RawQuery = target.RawQuery + req.URL.RawQuery
			} else {
				req.URL.RawQuery = target.RawQuery + "&" + req.URL.RawQuery
			}

			// 4. Set Host header to backend host.
			req.Host = target.Host

			// Log outgoing request (Debug — hot path).
			p.logger.Debug("Outgoing request to backend",
				zap.String("method", req.Method),
				zap.String("url", req.URL.String()),
				zap.String("host", req.Host),
				zap.String("path", req.URL.Path),
				zap.String("raw_query", req.URL.RawQuery),
			)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			target, _ := r.Context().Value(targetContextKey).(*url.URL)
			backendHost := ""
			if target != nil {
				backendHost = target.Host
			}
			p.logger.Error("Reverse proxy error",
				zap.Error(err),
				zap.String("path", r.URL.Path),
				zap.String("backend", backendHost),
			)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
	}

	p.reverseProxy = rp
	return p
}

// ServeHTTP handles the proxy request
func (p *HTTPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// H3 FIX: per-request logs demoted to Debug.
	p.logger.Debug("Proxy request received",
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
		zap.String("type", p.endpointType),
		zap.Bool("websocket", IsWebSocketRequest(r)),
	)

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

	// H3 FIX: per-request logs demoted to Debug.
	p.logger.Debug("Routing decision made",
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

	// Handle WebSocket upgrade requests separately.
	// H9 FIX: pass nodeMetrics so handleWebSocket doesn't call GetBestNode again.
	if IsWebSocketRequest(r) {
		p.handleWebSocket(w, r, target, nodeName, network, start, decision, nodeMetrics)
		return
	}

	// H1 FIX: store target in context so the shared Director can read it.
	ctx := context.WithValue(r.Context(), targetContextKey, target)
	r = r.WithContext(ctx)

	// Apply per-request timeout via context (dynamic, race-free).
	timeoutNs := p.proxyTimeoutNs.Load()
	if timeoutNs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(r.Context(), time.Duration(timeoutNs))
		defer cancel()
		r = r.WithContext(ctx)
	}

	// Wrap response writer to track status and size
	tracker := &responseTracker{ResponseWriter: w, statusCode: 200}

	// H3 FIX: per-request logs demoted to Debug.
	p.logger.Debug("Proxying to backend",
		zap.String("backend_host", target.Host),
		zap.String("backend_scheme", target.Scheme),
		zap.String("request_path", r.URL.Path),
		zap.String("request_query", r.URL.RawQuery),
	)
	p.reverseProxy.ServeHTTP(tracker, r)

	// H3 FIX: per-request log demoted to Debug.
	p.logger.Debug("Backend response received",
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
			p.logger.Debug("Tracked 5xx error for external endpoint",
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

// handleWebSocket handles WebSocket proxy requests.
// Delegates the actual proxying to the shared ProxyWebSocket function,
// then records metrics based on the outcome.
func (p *HTTPProxy) handleWebSocket(w http.ResponseWriter, r *http.Request, target *url.URL, nodeName, network string, start time.Time, decision *selector.SelectionDecision, nodeMetrics *storage.NodeMetrics) {
	p.logger.Debug("Handling WebSocket upgrade",
		zap.String("node", nodeName),
		zap.String("network", network),
	)

	if !nodeMetrics.WebSocketAvailable {
		p.logger.Warn("Selected node does not support WebSocket",
			zap.String("node", nodeName),
			zap.String("network", network),
		)
		http.Error(w, "WebSocket not supported by selected backend", http.StatusServiceUnavailable)
		metrics.ProxyErrors.WithLabelValues(network, nodeName, p.endpointType, "503", "websocket_not_supported").Inc()
		return
	}

	statusCode, err := ProxyWebSocket(w, r, target.String(), p.logger)
	duration := time.Since(start)

	statusStr := strconv.Itoa(statusCode)
	metrics.ProxyRequestDuration.WithLabelValues(network, nodeName, p.endpointType, statusStr).Observe(duration.Seconds())
	metrics.NodeRequests.WithLabelValues(network, nodeName, p.endpointType, "WEBSOCKET").Inc()

	if err != nil {
		// Extract granular error category from WSProxyError.
		errCategory := "websocket_error"
		if wsErr, ok := err.(*WSProxyError); ok {
			errCategory = wsErr.Category
		}
		p.logger.Debug("WebSocket proxy error",
			zap.String("node", nodeName),
			zap.String("category", errCategory),
			zap.Error(err),
			zap.Duration("duration", duration),
		)
		metrics.ProxyErrors.WithLabelValues(network, nodeName, p.endpointType, statusStr, errCategory).Inc()
	} else {
		p.logger.Debug("WebSocket proxied",
			zap.String("network", network),
			zap.String("node", nodeName),
			zap.String("type", p.endpointType),
			zap.Duration("duration", duration),
			zap.String("selection_reason", decision.Reason),
		)
	}
}
