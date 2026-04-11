package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"sauron/config"
	"sauron/metrics"
	"sauron/selector"
	"sauron/storage"

	"go.uber.org/zap"
)

// maxRequestBodySize is the maximum body size buffered for retry support (10 MB).
const maxRequestBodySize = 10 * 1024 * 1024

// errRetriable is returned by forwardRequest when the attempt should be retried.
var errRetriable = errors.New("retriable error")

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

// HTTPProxy handles HTTP/API and RPC proxying with retry and circuit-breaking support.
// The gates through which the Ringwraiths pass
type HTTPProxy struct {
	selector       *selector.Selector
	configLoader   *config.MultiChainLoader
	endpointStore  *storage.ExternalEndpointStore
	transport      *http.Transport
	circuitBreaker *CircuitBreaker // shared circuit breaker (can be nil)
	logger         *zap.Logger
	endpointType   string // V2 protocol name: "rest", "rpc", "jsonrpc", "http", etc.
	network        string // The network this proxy serves
}

// NewHTTPProxy creates a new HTTP proxy for a specific network.
func NewHTTPProxy(
	sel *selector.Selector,
	configLoader *config.MultiChainLoader,
	endpointStore *storage.ExternalEndpointStore,
	logger *zap.Logger,
	endpointType string,
	network string,
	circuitBreaker *CircuitBreaker,
) *HTTPProxy {
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

	return &HTTPProxy{
		selector:       sel,
		configLoader:   configLoader,
		endpointStore:  endpointStore,
		transport:      transport,
		circuitBreaker: circuitBreaker,
		logger:         logger,
		endpointType:   endpointType,
		network:        network,
	}
}

// ServeHTTP handles the proxy request with retry and circuit-breaker support.
func (p *HTTPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	network := p.network

	p.logger.Debug("Proxy request received",
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
		zap.String("type", p.endpointType),
		zap.Bool("websocket", IsWebSocketRequest(r)),
	)

	// Buffer request body for potential retry.
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Determine retry budget from circuit breaker config.
	cbCfg := p.getCircuitBreakerConfig()
	maxAttempts := 1
	if cbCfg != nil && cbCfg.RetryAttempts > 0 {
		maxAttempts += cbCfg.RetryAttempts
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 && cbCfg != nil && cbCfg.RetryBackoff > 0 {
			time.Sleep(cbCfg.RetryBackoff)
		}

		// Select best node for this attempt.
		nodeMetrics, nodeName, decision := p.selector.GetBestNode(network, p.endpointType)
		if nodeMetrics == nil || nodeName == "" {
			if attempt == 0 {
				p.logger.Warn("No available nodes for routing",
					zap.String("network", network),
					zap.String("type", p.endpointType),
				)
				http.Error(w, "No available nodes", http.StatusServiceUnavailable)
				return
			}
			// No more nodes — fall through to 502.
			break
		}

		// Get endpoint URL for selected node.
		targetURL := p.selector.GetEndpointURL(nodeName, p.endpointType)
		if targetURL == "" {
			p.logger.Error("Failed to get endpoint URL",
				zap.String("node", nodeName),
				zap.String("type", p.endpointType),
			)
			continue
		}

		p.logger.Debug("Routing decision made",
			zap.String("network", network),
			zap.String("selected_node", nodeName),
			zap.String("target_url", targetURL),
			zap.String("path", r.URL.Path),
			zap.String("attempt", strconv.Itoa(attempt+1)+"/"+strconv.Itoa(maxAttempts)),
		)

		// WebSocket upgrades are handled separately (no retry on WS).
		if IsWebSocketRequest(r) {
			target, parseErr := url.Parse(targetURL)
			if parseErr != nil {
				p.logger.Error("Failed to parse target URL for WebSocket",
					zap.String("url", targetURL),
					zap.Error(parseErr),
				)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			p.handleWebSocket(w, r, target, nodeName, network, start, decision, nodeMetrics)
			return
		}

		// Forward the request. forwardRequest writes the response to w on
		// success (returns nil error). On retriable failure it returns errRetriable
		// without touching w. On connection error it also returns errRetriable.
		statusCode, fwdErr := p.forwardRequest(w, r, targetURL, bodyBytes, nodeName, network, start, decision, cbCfg)

		if fwdErr != nil {
			// Record circuit-breaker error for connection failures and retriable codes.
			if p.circuitBreaker != nil && cbCfg != nil {
				p.circuitBreaker.RecordError(network, nodeName, p.endpointType, cbCfg.Threshold)
			}
			// Record metric for the failed attempt.
			if statusCode > 0 {
				metrics.ProxyErrors.WithLabelValues(network, nodeName, p.endpointType, strconv.Itoa(statusCode), "retriable").Inc()
			}
			p.logger.Debug("Retriable error on attempt",
				zap.String("network", network),
				zap.String("node", nodeName),
				zap.Int("attempt", attempt+1),
				zap.Int("status_code", statusCode),
				zap.Error(fwdErr),
			)
			continue
		}

		// Success — circuit breaker reset, metrics already recorded in forwardRequest.
		if p.circuitBreaker != nil {
			p.circuitBreaker.RecordSuccess(network, nodeName, p.endpointType)
		}
		return
	}

	// All attempts exhausted.
	http.Error(w, "Bad Gateway", http.StatusBadGateway)
}

// forwardRequest forwards the HTTP request to the backend.
//
// On success or non-retriable response: writes headers+body to w, records all
// metrics, and returns (statusCode, nil).
//
// On connection error or retriable status code: discards the backend response
// (to allow connection reuse), records no response metrics, and returns
// (statusCode, errRetriable) so ServeHTTP can retry.
func (p *HTTPProxy) forwardRequest(
	w http.ResponseWriter,
	r *http.Request,
	targetURL string,
	body []byte,
	nodeName, network string,
	start time.Time,
	decision *selector.SelectionDecision,
	cbCfg *config.CircuitBreakerConfig,
) (int, error) {
	target, err := url.Parse(targetURL)
	if err != nil {
		return 0, errRetriable
	}

	// Build outgoing URL — replicate full httputil.NewSingleHostReverseProxy Director behavior.
	outURL := *target
	outURL.Path = singleJoiningSlash(target.Path, r.URL.Path)
	if target.Path != "" && r.URL.RawPath != "" {
		outURL.RawPath = singleJoiningSlash(target.RawPath, r.URL.RawPath)
	}
	if target.RawQuery == "" || r.URL.RawQuery == "" {
		outURL.RawQuery = target.RawQuery + r.URL.RawQuery
	} else {
		outURL.RawQuery = target.RawQuery + "&" + r.URL.RawQuery
	}

	// Create outgoing request with the buffered body.
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, outURL.String(), bytes.NewReader(body))
	if err != nil {
		return 0, errRetriable
	}

	// Copy headers from original request.
	for k, vv := range r.Header {
		for _, v := range vv {
			outReq.Header.Add(k, v)
		}
	}
	// Override Host to match the backend.
	outReq.Header.Set("Host", target.Host)
	outReq.Host = target.Host

	p.logger.Debug("Outgoing request to backend",
		zap.String("method", outReq.Method),
		zap.String("url", outReq.URL.String()),
		zap.String("host", outReq.Host),
		zap.String("path", outReq.URL.Path),
		zap.String("raw_query", outReq.URL.RawQuery),
	)

	// Execute the request via the shared transport.
	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		p.logger.Error("Reverse proxy error",
			zap.Error(err),
			zap.String("path", r.URL.Path),
			zap.String("backend", target.Host),
		)
		return 0, errRetriable
	}
	defer func() { _ = resp.Body.Close() }()

	// Check if this status code should trigger a retry.
	if cbCfg != nil && isRetriableHTTPCode(resp.StatusCode, cbCfg.HTTPCodes) {
		// Drain body to allow connection reuse.
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, errRetriable
	}

	// Non-retriable response — write it to the client.
	// Copy response headers.
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	bytesWritten, _ := io.Copy(w, resp.Body)

	p.logger.Debug("Backend response received",
		zap.Int("status_code", resp.StatusCode),
		zap.Int64("response_bytes", bytesWritten),
	)

	// Record metrics.
	duration := time.Since(start)
	statusStr := strconv.Itoa(resp.StatusCode)

	metrics.ProxyRequestDuration.WithLabelValues(
		network,
		nodeName,
		p.endpointType,
		statusStr,
	).Observe(duration.Seconds())

	metrics.ProxyResponseSize.WithLabelValues(network, p.endpointType).Observe(float64(bytesWritten))
	metrics.NodeRequests.WithLabelValues(network, nodeName, p.endpointType, r.Method).Inc()

	if resp.StatusCode >= 400 {
		metrics.ProxyErrors.WithLabelValues(network, nodeName, p.endpointType, statusStr, "http_error").Inc()
	}

	// Track 5xx errors for external endpoint health.
	if resp.StatusCode >= 500 && p.endpointStore != nil {
		if p.endpointStore.TrackProxyError(network, p.endpointType, targetURL) {
			p.logger.Debug("Tracked 5xx error for external endpoint",
				zap.String("url", targetURL),
				zap.String("network", network),
				zap.String("type", p.endpointType),
				zap.Int("status", resp.StatusCode),
			)
		}
	}

	p.logger.Debug("Request proxied",
		zap.String("network", network),
		zap.String("node", nodeName),
		zap.String("type", p.endpointType),
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
		zap.Int("status", resp.StatusCode),
		zap.Int64("bytes", bytesWritten),
		zap.Duration("duration", duration),
		zap.String("selection_reason", decision.Reason),
	)

	return resp.StatusCode, nil
}

// isRetriableHTTPCode reports whether code is in the retriable set.
func isRetriableHTTPCode(code int, retriableCodes []int) bool {
	for _, c := range retriableCodes {
		if code == c {
			return true
		}
	}
	return false
}

// getCircuitBreakerConfig returns the CircuitBreakerConfig for this proxy's network.
// Returns nil when unconfigured.
func (p *HTTPProxy) getCircuitBreakerConfig() *config.CircuitBreakerConfig {
	cfg := p.configLoader.Get()
	net := cfg.FindNetwork(p.network)
	if net == nil {
		return nil
	}
	return net.CircuitBreaker
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

		// Record circuit-breaker error for WebSocket backend failures.
		cbCfg := p.getCircuitBreakerConfig()
		if p.circuitBreaker != nil && cbCfg != nil {
			p.circuitBreaker.RecordError(network, nodeName, p.endpointType, cbCfg.Threshold)
		}
	} else {
		p.logger.Debug("WebSocket proxied",
			zap.String("network", network),
			zap.String("node", nodeName),
			zap.String("type", p.endpointType),
			zap.Duration("duration", duration),
			zap.String("selection_reason", decision.Reason),
		)

		// Record circuit-breaker success for WebSocket.
		if p.circuitBreaker != nil {
			p.circuitBreaker.RecordSuccess(network, nodeName, p.endpointType)
		}
	}
}
