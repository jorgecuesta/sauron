package storage

import (
	"sync"
	"time"

	"sauron/metrics"

	"go.uber.org/zap"
)

// ExternalEndpoint represents a single external Sauron endpoint with validation state
type ExternalEndpoint struct {
	URL          string // Advertised URL
	Network      string // Network (pocket, pocket-beta, etc.)
	Type         string // Type (api, rpc, grpc)
	ExternalName string // External Sauron name (e.g., "pnf")
	RingURL      string // Which ring advertised this endpoint

	// Validation state
	IsValidated        bool      // Passed validation check
	IsWorking          bool      // Currently healthy (not failed)
	ErrorCount         int       // Consecutive proxy errors (5xx only)
	LastValidated      time.Time // Last successful validation
	LastError          time.Time // Last error timestamp
	WebSocketAvailable bool      // Whether WebSocket endpoint is working (RPC only)

	// Metrics
	Height  int64         // Latest height
	Latency time.Duration // Latest latency
}

// ExternalEndpointStore manages external Sauron endpoints
// Thread-safe storage for tracking advertised endpoints and their validation state
type ExternalEndpointStore struct {
	mu        sync.RWMutex
	endpoints map[string]*ExternalEndpoint // key: "{externalName}:{ring}:{network}:{type}:{url}"
	logger    *zap.Logger
}

// NewExternalEndpointStore creates a new external endpoint store
func NewExternalEndpointStore(logger *zap.Logger) *ExternalEndpointStore {
	return &ExternalEndpointStore{
		endpoints: make(map[string]*ExternalEndpoint),
		logger:    logger,
	}
}

// makeKey creates a unique key for an endpoint
func (s *ExternalEndpointStore) makeKey(externalName, ringURL, network, endpointType, url string) string {
	return externalName + ":" + ringURL + ":" + network + ":" + endpointType + ":" + url
}

// StoreAdvertised stores an advertised endpoint (may not be validated yet)
func (s *ExternalEndpointStore) StoreAdvertised(externalName, ringURL, network, endpointType, url string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := s.makeKey(externalName, ringURL, network, endpointType, url)

	// Check if already exists
	if ep, exists := s.endpoints[key]; exists {
		// Update existing endpoint
		ep.URL = url
		s.logger.Debug("Updated advertised endpoint",
			zap.String("external", externalName),
			zap.String("ring", ringURL),
			zap.String("network", network),
			zap.String("type", endpointType),
			zap.String("url", url),
		)
		return
	}

	// Create new endpoint
	s.endpoints[key] = &ExternalEndpoint{
		URL:          url,
		Network:      network,
		Type:         endpointType,
		ExternalName: externalName,
		RingURL:      ringURL,
		IsValidated:  false, // Not validated yet
		IsWorking:    false, // Not working until validated
		ErrorCount:   0,
	}

	s.logger.Info("Stored new advertised endpoint",
		zap.String("external", externalName),
		zap.String("ring", ringURL),
		zap.String("network", network),
		zap.String("type", endpointType),
		zap.String("url", url),
	)
}

// MarkValidated marks an endpoint as validated and working
func (s *ExternalEndpointStore) MarkValidated(externalName, ringURL, network, endpointType, url string, height int64, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := s.makeKey(externalName, ringURL, network, endpointType, url)
	ep, exists := s.endpoints[key]
	if !exists {
		s.logger.Warn("Attempted to validate non-existent endpoint",
			zap.String("external", externalName),
			zap.String("network", network),
			zap.String("type", endpointType),
			zap.String("url", url),
		)
		return
	}

	wasValidated := ep.IsValidated
	wasFailed := ep.IsValidated && !ep.IsWorking
	ep.IsValidated = true
	ep.IsWorking = true
	ep.ErrorCount = 0
	ep.LastValidated = time.Now()
	ep.Height = height
	ep.Latency = latency

	if wasFailed {
		// FAILED -> working is a recovery, whichever check saw it first (the
		// recovery loop or the ring's own revalidation).
		metrics.ExternalEndpointRecoveries.WithLabelValues(network, endpointType, externalName).Inc()
		s.logger.Info("Failed endpoint has recovered",
			zap.String("external", externalName),
			zap.String("ring", ringURL),
			zap.String("network", network),
			zap.String("type", endpointType),
			zap.String("url", url),
			zap.Duration("latency", latency),
		)
	} else if !wasValidated {
		s.logger.Info("Endpoint validated successfully",
			zap.String("external", externalName),
			zap.String("ring", ringURL),
			zap.String("network", network),
			zap.String("type", endpointType),
			zap.String("url", url),
			zap.Int64("height", height),
			zap.Duration("latency", latency),
		)
	} else {
		s.logger.Debug("Endpoint revalidated",
			zap.String("external", externalName),
			zap.String("network", network),
			zap.String("type", endpointType),
			zap.Int64("height", height),
		)
	}

	// Record metrics
	metrics.ExternalEndpointValidationAttempts.WithLabelValues(network, endpointType, externalName, "success").Inc()
	metrics.ExternalEndpointValidationLatency.WithLabelValues(network, endpointType, externalName).Observe(latency.Seconds())
	s.setErrorCountGauge(externalName, network, endpointType)
}

// MarkValidationFailed marks an endpoint validation as failed
func (s *ExternalEndpointStore) MarkValidationFailed(externalName, ringURL, network, endpointType, url string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := s.makeKey(externalName, ringURL, network, endpointType, url)
	ep, exists := s.endpoints[key]
	if !exists {
		return
	}

	// An endpoint that had passed validation is now FAILED: still validated,
	// so the recovery loop re-checks it, but not working. One that never
	// passed stays unvalidated.
	ep.IsWorking = false
	ep.LastError = time.Now()
	if ep.IsValidated {
		ep.ErrorCount++
		s.setErrorCountGauge(externalName, network, endpointType)
	}

	s.logger.Warn("Endpoint validation failed",
		zap.String("external", externalName),
		zap.String("ring", ringURL),
		zap.String("network", network),
		zap.String("type", endpointType),
		zap.String("url", url),
	)

	// Record metrics
	metrics.ExternalEndpointValidationAttempts.WithLabelValues(network, endpointType, externalName, "failure").Inc()
}

// GetValidatedEndpoints returns all validated+working endpoints for a network/type
func (s *ExternalEndpointStore) GetValidatedEndpoints(network, endpointType string) []*ExternalEndpoint {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var validated []*ExternalEndpoint
	for _, ep := range s.endpoints {
		if ep.Network == network && ep.Type == endpointType && ep.IsValidated && ep.IsWorking {
			// Create a copy to avoid race conditions
			epCopy := *ep
			validated = append(validated, &epCopy)
		}
	}

	return validated
}

// GetFailedEndpoints returns all failed endpoints (for health check recovery)
func (s *ExternalEndpointStore) GetFailedEndpoints() []*ExternalEndpoint {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var failed []*ExternalEndpoint
	for _, ep := range s.endpoints {
		if ep.IsValidated && !ep.IsWorking {
			// Create a copy
			epCopy := *ep
			failed = append(failed, &epCopy)
		}
	}

	return failed
}

// TrackProxyError tracks a proxy error for an endpoint identified by URL
// Returns true if the endpoint was found and error was tracked
func (s *ExternalEndpointStore) TrackProxyError(network, endpointType, url string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Find the endpoint by matching network, type, and URL
	for _, ep := range s.endpoints {
		if ep.Network == network && ep.Type == endpointType && ep.URL == url {
			ep.ErrorCount++
			ep.LastError = time.Now()

			if ep.ErrorCount >= 3 && ep.IsWorking {
				ep.IsWorking = false
				s.logger.Warn("External endpoint marked as not working due to proxy errors",
					zap.String("external", ep.ExternalName),
					zap.String("ring", ep.RingURL),
					zap.String("network", network),
					zap.String("type", endpointType),
					zap.String("url", url),
					zap.Int("error_count", ep.ErrorCount),
				)
			} else {
				s.logger.Debug("External endpoint proxy error tracked",
					zap.String("external", ep.ExternalName),
					zap.String("network", network),
					zap.String("type", endpointType),
					zap.String("url", url),
					zap.Int("error_count", ep.ErrorCount),
				)
			}

			// Record metrics
			metrics.ExternalEndpointProxyErrors.WithLabelValues(network, endpointType, ep.ExternalName).Inc()
			s.setErrorCountGauge(ep.ExternalName, network, endpointType)

			return true
		}
	}

	return false
}

// UpdateWebSocketAvailability updates the WebSocket availability status for an RPC endpoint
func (s *ExternalEndpointStore) UpdateWebSocketAvailability(externalName, ringURL, network, endpointType, url string, available bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := s.makeKey(externalName, ringURL, network, endpointType, url)
	ep, exists := s.endpoints[key]
	if !exists {
		return
	}

	ep.WebSocketAvailable = available

	s.logger.Debug("Updated WebSocket availability for external endpoint",
		zap.String("external", externalName),
		zap.String("network", network),
		zap.String("type", endpointType),
		zap.String("url", url),
		zap.Bool("available", available),
	)
}

// setErrorCountGauge publishes the highest error count among externalName's
// endpoints of network and endpointType. Callers hold s.mu.
func (s *ExternalEndpointStore) setErrorCountGauge(externalName, network, endpointType string) {
	highest := 0
	for _, ep := range s.endpoints {
		if ep.ExternalName == externalName && ep.Network == network && ep.Type == endpointType && ep.ErrorCount > highest {
			highest = ep.ErrorCount
		}
	}
	metrics.ExternalEndpointErrorCount.WithLabelValues(network, endpointType, externalName).Set(float64(highest))
}

// UpdateAggregateMetrics updates aggregate endpoint count metrics
// Should be called periodically (e.g., every 10 seconds) to avoid overhead
func (s *ExternalEndpointStore) UpdateAggregateMetrics() {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Group by external/network/type
	type key struct {
		external string
		network  string
		typ      string
	}

	counts := make(map[key]struct {
		tracked   int
		validated int
		working   int
	})

	for _, ep := range s.endpoints {
		k := key{external: ep.ExternalName, network: ep.Network, typ: ep.Type}
		count := counts[k]
		count.tracked++
		if ep.IsValidated {
			count.validated++
		}
		if ep.IsWorking {
			count.working++
		}
		counts[k] = count
	}

	// Update metrics
	for k, count := range counts {
		metrics.ExternalEndpointsTracked.WithLabelValues(k.network, k.typ, k.external).Set(float64(count.tracked))
		metrics.ExternalEndpointsValidated.WithLabelValues(k.network, k.typ, k.external).Set(float64(count.validated))
		metrics.ExternalEndpointsWorking.WithLabelValues(k.network, k.typ, k.external).Set(float64(count.working))
	}
}

// EndpointsForRing returns copies of every endpoint externalName's ringURL
// advertised for network, whatever its state.
func (s *ExternalEndpointStore) EndpointsForRing(externalName, ringURL, network string) []*ExternalEndpoint {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var eps []*ExternalEndpoint
	for _, ep := range s.endpoints {
		if ep.ExternalName == externalName && ep.RingURL == ringURL && ep.Network == network {
			epCopy := *ep
			eps = append(eps, &epCopy)
		}
	}
	return eps
}
