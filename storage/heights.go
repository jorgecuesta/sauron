package storage

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
)

const (
	// LatencyHistorySize is the number of latency measurements to keep for averaging
	LatencyHistorySize = 10
)

// NodeMetrics stores height and latency information for a node
// The Dark Lord's memory of each kingdom
type NodeMetrics struct {
	Height             int64
	Timestamp          time.Time
	Source             string // "internal" or "external"
	LatencyHistory     []time.Duration
	AvgLatency         time.Duration
	WebSocketAvailable bool // Whether WebSocket endpoint is working
	mu                 sync.Mutex
}

// HeightStore manages all node metrics using xsync for thread-safe access
// The archives of Barad-dûr
type HeightStore struct {
	data *xsync.Map[string, *NodeMetrics]
}

// NewHeightStore creates a new height store
func NewHeightStore() *HeightStore {
	return &HeightStore{
		data: xsync.NewMap[string, *NodeMetrics](),
	}
}

// makeKey creates a unique key for a node and endpoint type
// Format: "network:node:type"
func makeKey(network, node, endpointType string) string {
	return fmt.Sprintf("%s:%s:%s", network, node, endpointType)
}

// Update stores or updates the height and latency for a node
func (s *HeightStore) Update(network, node, endpointType string, height int64, latency time.Duration, source string) {
	key := makeKey(network, node, endpointType)

	// Get or create metrics
	metrics, _ := s.data.LoadOrStore(key, &NodeMetrics{
		LatencyHistory: make([]time.Duration, 0, LatencyHistorySize),
	})

	metrics.mu.Lock()
	defer metrics.mu.Unlock()

	// Update height and timestamp
	metrics.Height = height
	metrics.Timestamp = time.Now()
	metrics.Source = source

	// Update latency history (keep last N measurements)
	metrics.LatencyHistory = append(metrics.LatencyHistory, latency)
	if len(metrics.LatencyHistory) > LatencyHistorySize {
		metrics.LatencyHistory = metrics.LatencyHistory[1:]
	}

	// Calculate simple moving average
	var sum time.Duration
	for _, l := range metrics.LatencyHistory {
		sum += l
	}
	metrics.AvgLatency = sum / time.Duration(len(metrics.LatencyHistory))
}

// Get retrieves the metrics for a specific node
func (s *HeightStore) Get(network, node, endpointType string) (*NodeMetrics, bool) {
	key := makeKey(network, node, endpointType)
	metrics, ok := s.data.Load(key)
	if !ok {
		return nil, false
	}

	// Return a copy to prevent external modifications
	metrics.mu.Lock()
	defer metrics.mu.Unlock()

	copy := &NodeMetrics{
		Height:             metrics.Height,
		Timestamp:          metrics.Timestamp,
		Source:             metrics.Source,
		LatencyHistory:     make([]time.Duration, len(metrics.LatencyHistory)),
		AvgLatency:         metrics.AvgLatency,
		WebSocketAvailable: metrics.WebSocketAvailable,
	}
	copyDurations(copy.LatencyHistory, metrics.LatencyHistory)

	return copy, true
}

// GetByNetwork returns all nodes for a given network and endpoint type
func (s *HeightStore) GetByNetwork(network, endpointType string) map[string]*NodeMetrics {
	result := make(map[string]*NodeMetrics)

	s.data.Range(func(keyStr string, metrics *NodeMetrics) bool {
		// Parse key: "network:node:type"
		if keyNetwork, keyNode, keyType := parseKey(keyStr); keyNetwork == network && keyType == endpointType {
			metrics.mu.Lock()
			copy := &NodeMetrics{
				Height:             metrics.Height,
				Timestamp:          metrics.Timestamp,
				Source:             metrics.Source,
				LatencyHistory:     make([]time.Duration, len(metrics.LatencyHistory)),
				AvgLatency:         metrics.AvgLatency,
				WebSocketAvailable: metrics.WebSocketAvailable,
			}
			copyDurations(copy.LatencyHistory, metrics.LatencyHistory)
			metrics.mu.Unlock()

			result[keyNode] = copy
		}
		return true
	})

	return result
}

// GetAllNetworks returns a list of all networks being monitored
func (s *HeightStore) GetAllNetworks() []string {
	networks := make(map[string]bool)

	s.data.Range(func(keyStr string, _ *NodeMetrics) bool {
		if network, _, _ := parseKey(keyStr); network != "" {
			networks[network] = true
		}
		return true
	})

	result := make([]string, 0, len(networks))
	for network := range networks {
		result = append(result, network)
	}
	return result
}

// GetHighestHeight returns the highest height for a given network and endpoint type
func (s *HeightStore) GetHighestHeight(network, endpointType string) int64 {
	var maxHeight int64

	s.data.Range(func(keyStr string, metrics *NodeMetrics) bool {
		if keyNetwork, _, keyType := parseKey(keyStr); keyNetwork == network && keyType == endpointType {
			metrics.mu.Lock()
			if metrics.Height > maxHeight {
				maxHeight = metrics.Height
			}
			metrics.mu.Unlock()
		}
		return true
	})

	return maxHeight
}

// parseKey splits a key into its components
// Format: "network:node:type"
func parseKey(key string) (network, node, endpointType string) {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) == 3 {
		return parts[0], parts[1], parts[2]
	}
	return "", "", ""
}

// copyDurations copies duration slice elements from src to dst
func copyDurations(dst, src []time.Duration) {
	for i := 0; i < len(src) && i < len(dst); i++ {
		dst[i] = src[i]
	}
}

// UpdateWebSocketAvailability updates the WebSocket availability status for a node
func (s *HeightStore) UpdateWebSocketAvailability(network, node, endpointType string, available bool) {
	key := makeKey(network, node, endpointType)

	// Get or create metrics
	metrics, _ := s.data.LoadOrStore(key, &NodeMetrics{
		LatencyHistory: make([]time.Duration, 0, LatencyHistorySize),
	})

	metrics.mu.Lock()
	defer metrics.mu.Unlock()

	metrics.WebSocketAvailable = available
}
