package status

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"sauron/config"
	"sauron/selector"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// Handler provides the status API endpoints
// The Palantír - how others peer into this tower
type Handler struct {
	selector     *selector.Selector
	configLoader *config.Loader
	logger       *zap.Logger
	rateLimiter  *RateLimiter
}

// StatusResponse represents the response format
// Returns the maximum height and advertised endpoints for connecting to this Sauron
type StatusResponse struct {
	Height       int64  `json:"height"`                  // Maximum height across all endpoint types
	API          string `json:"api,omitempty"`           // Advertised API endpoint URL
	RPC          string `json:"rpc,omitempty"`           // Advertised RPC endpoint URL
	GRPC         string `json:"grpc,omitempty"`          // Advertised gRPC endpoint URL
	GRPCInsecure bool   `json:"grpc_insecure,omitempty"` // Whether advertised gRPC endpoint uses insecure (no TLS)
}

// NewHandler creates a new status handler
func NewHandler(selector *selector.Selector, configLoader *config.Loader, logger *zap.Logger) *Handler {
	cfg := configLoader.Get()

	var rateLimiter *RateLimiter
	if cfg.RateLimit.Enabled {
		// Set defaults if not configured
		reqPerSec := cfg.RateLimit.RequestsPerSecond
		if reqPerSec == 0 {
			reqPerSec = 10 // default: 10 requests per second
		}
		burst := cfg.RateLimit.Burst
		if burst == 0 {
			burst = reqPerSec * 2 // default: 2x burst
		}

		rateLimiter = NewRateLimiter(reqPerSec, burst, cfg.RateLimit.TrustProxy)
		logger.Info("Rate limiting enabled",
			zap.Int("requests_per_second", reqPerSec),
			zap.Int("burst", burst),
			zap.Bool("trust_proxy", cfg.RateLimit.TrustProxy),
		)
	}

	return &Handler{
		selector:     selector,
		configLoader: configLoader,
		logger:       logger,
		rateLimiter:  rateLimiter,
	}
}

// SetupRoutes configures all status API routes
func (h *Handler) SetupRoutes(mux *http.ServeMux) {
	cfg := h.configLoader.Get()

	// Prometheus metrics endpoint (no auth required)
	mux.Handle("/metrics", promhttp.Handler())

	// Health check (no auth required)
	mux.HandleFunc("/health", h.handleHealth)

	// Readiness check (no auth required)
	mux.HandleFunc("/ready", h.handleReady)

	// Status endpoint (with optional request ID, auth, and rate limiting)
	var statusHandler http.Handler = http.HandlerFunc(h.handleStatus)

	// Apply request ID middleware (outermost - all requests get an ID)
	statusHandler = h.requestIDMiddleware(statusHandler)

	// Apply auth middleware if enabled
	if cfg.Auth {
		statusHandler = h.authMiddleware(statusHandler)
	}

	// Apply rate limiting middleware if enabled
	if h.rateLimiter != nil {
		statusHandler = h.rateLimitMiddleware(statusHandler)
	}

	mux.Handle("/", statusHandler)
}

// requestIDMiddleware generates and attaches a unique request ID to each request
func (h *Handler) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if request already has an ID (from proxy/load balancer)
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			// Generate new UUID
			requestID = uuid.New().String()
		}

		// Store in context
		ctx := context.WithValue(r.Context(), contextKeyRequestID, requestID)

		// Set response header for client correlation
		w.Header().Set("X-Request-ID", requestID)

		// Continue with updated context
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// rateLimitMiddleware applies rate limiting to requests
func (h *Handler) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.rateLimiter.Allow(r) {
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			h.logger.Warn("Rate limit exceeded",
				zap.String("path", r.URL.Path),
				zap.String("method", r.Method),
				zap.String("remote_addr", r.RemoteAddr),
				zap.String("request_id", getRequestID(r)),
			)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Shutdown stops the rate limiter cleanup goroutine
func (h *Handler) Shutdown() {
	if h.rateLimiter != nil {
		h.rateLimiter.Stop()
	}
}

// handleStatus returns the highest heights for a network
// GET /{network}/status
func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	// Parse network from path: /{network}/status
	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")

	if len(parts) != 2 || parts[1] != "status" {
		http.Error(w, "Invalid request path. Expected format: /{network}/status", http.StatusNotFound)
		h.logger.Warn("Invalid status request path",
			zap.String("request_id", getRequestID(r)),
			zap.String("path", r.URL.Path),
		)
		return
	}

	network := parts[0]

	// Get user permissions from context (set by auth middleware)
	enabledTypes := h.getEnabledTypes(r)

	// Get highest heights for each endpoint type
	heights := h.selector.GetHighestHeights(network, enabledTypes)

	if len(heights) == 0 {
		msg := fmt.Sprintf("No height data available for network: %s", network)
		http.Error(w, msg, http.StatusNotFound)
		h.logger.Warn("No heights available",
			zap.String("request_id", getRequestID(r)),
			zap.String("network", network),
		)
		return
	}

	// Find maximum height across all endpoint types
	maxHeight := int64(0)
	for _, height := range heights {
		if height > maxHeight {
			maxHeight = height
		}
	}

	// Build response with maximum height and advertised endpoints
	cfg := h.configLoader.Get()
	resp := StatusResponse{
		Height: maxHeight,
	}

	// Find the network config to get advertised endpoints
	var networkConfig *config.Network
	for _, net := range cfg.Networks {
		if net.Name == network {
			networkConfig = &net
			break
		}
	}

	// Add advertised endpoints based on enabled types
	if networkConfig != nil {
		for _, endpointType := range enabledTypes {
			switch endpointType {
			case "api":
				if networkConfig.API != "" {
					resp.API = networkConfig.API
				}
			case "rpc":
				if networkConfig.RPC != "" {
					resp.RPC = networkConfig.RPC
				}
			case "grpc":
				if networkConfig.GRPC != "" {
					resp.GRPC = networkConfig.GRPC
					resp.GRPCInsecure = networkConfig.GRPCInsecure
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Error("Failed to encode status response",
			zap.String("request_id", getRequestID(r)),
			zap.Error(err),
		)
		http.Error(w, "Failed to encode response. Please try again later.", http.StatusInternalServerError)
		return
	}

	h.logger.Debug("Status request served",
		zap.String("request_id", getRequestID(r)),
		zap.String("network", network),
		zap.Int64("height", resp.Height),
		zap.String("api", resp.API),
		zap.String("rpc", resp.RPC),
		zap.String("grpc", resp.GRPC),
	)
}

// handleHealth returns 200 if the service is running
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// handleReady returns 200 if height checks are working
func (h *Handler) handleReady(w http.ResponseWriter, r *http.Request) {
	// Simple readiness check: are we tracking any heights?
	cfg := h.configLoader.Get()
	if len(cfg.Internals) == 0 {
		http.Error(w, "Service not ready: no internal nodes configured", http.StatusServiceUnavailable)
		h.logger.Warn("Readiness check failed: no internal nodes",
			zap.String("request_id", getRequestID(r)),
		)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Ready"))
}

// getRequestID extracts the request ID from context
func getRequestID(r *http.Request) string {
	if id, ok := r.Context().Value(contextKeyRequestID).(string); ok {
		return id
	}
	return "unknown"
}

// getEnabledTypes returns the enabled endpoint types for the request
// If auth is enabled, returns user-specific types from context
// Otherwise, returns globally enabled types
func (h *Handler) getEnabledTypes(r *http.Request) []string {
	cfg := h.configLoader.Get()

	// If auth is enabled, get types from context (set by auth middleware)
	if cfg.Auth {
		if types, ok := r.Context().Value(contextKeyEnabledTypes).([]string); ok {
			return types
		}
	}

	// Return globally enabled types
	return cfg.GetEnabledTypes()
}
