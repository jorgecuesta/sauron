package proxy

import (
	"fmt"
	"sync/atomic"

	"sauron/storage"

	"github.com/puzpuzpuz/xsync/v4"
	"go.uber.org/zap"
)

// CircuitBreaker tracks consecutive errors per node and marks nodes unhealthy
// after exceeding a threshold. Lock-free via xsync.Map + atomic counters.
type CircuitBreaker struct {
	healthStore *storage.HealthStore
	logger      *zap.Logger
	errors      *xsync.Map[string, *atomic.Int32]
}

// NewCircuitBreaker creates a new circuit breaker backed by the given HealthStore.
func NewCircuitBreaker(healthStore *storage.HealthStore, logger *zap.Logger) *CircuitBreaker {
	return &CircuitBreaker{
		healthStore: healthStore,
		logger:      logger,
		errors:      xsync.NewMap[string, *atomic.Int32](),
	}
}

// RecordError increments the consecutive error count for a node.
// If the count reaches the threshold, the node is marked unhealthy in the HealthStore.
func (cb *CircuitBreaker) RecordError(network, node, protocol string, threshold int) {
	if cb == nil || threshold <= 0 {
		return
	}

	key := cbKey(network, node, protocol)
	counter, _ := cb.errors.LoadOrCompute(key, func() (*atomic.Int32, bool) {
		return &atomic.Int32{}, false
	})

	count := int(counter.Add(1))

	if count >= threshold {
		reason := fmt.Sprintf("circuit breaker: %d consecutive errors", count)
		cb.healthStore.SetUnhealthy(network, node, protocol, reason)
		cb.logger.Info("Circuit breaker tripped — node marked unhealthy",
			zap.String("network", network),
			zap.String("node", node),
			zap.String("protocol", protocol),
			zap.Int("errors", count),
			zap.Int("threshold", threshold),
		)
	}
}

// RecordSuccess resets the consecutive error count for a node.
// Called on every successful proxied request to indicate the node is responsive.
func (cb *CircuitBreaker) RecordSuccess(network, node, protocol string) {
	if cb == nil {
		return
	}

	key := cbKey(network, node, protocol)
	if counter, ok := cb.errors.Load(key); ok {
		counter.Store(0)
	}
}

// GetErrorCount returns the current consecutive error count for a node.
// Used in tests and debugging.
func (cb *CircuitBreaker) GetErrorCount(network, node, protocol string) int {
	if cb == nil {
		return 0
	}

	key := cbKey(network, node, protocol)
	if counter, ok := cb.errors.Load(key); ok {
		return int(counter.Load())
	}
	return 0
}

func cbKey(network, node, protocol string) string {
	return network + ":" + node + ":" + protocol
}
