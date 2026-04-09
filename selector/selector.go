package selector

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"sauron/config"
	"sauron/internal/urlutil"
	"sauron/metrics"
	"sauron/storage"

	"go.uber.org/zap"
)

// Selector chooses the best node for a given network and endpoint type
// The Dark Lord's judgment - highest height → round-robin distribution
type Selector struct {
	store          *storage.HeightStore
	healthStore    *storage.HealthStore // Optional: if set, filters by protocol health
	endpointStore  *storage.ExternalEndpointStore
	archivalFilter *ArchivalFilter // Optional: archival node filtering
	syncFilter     *SyncFilter     // Optional: oracle drift filtering
	configLoader   *config.Loader
	logger         *zap.Logger
	rrCounters     sync.Map // map[string]*uint64 — per "network:type" round-robin counter
}

// nodeWithName pairs a node name with its metrics for internal processing.
type nodeWithName struct {
	name    string
	metrics *storage.NodeMetrics
}

// SelectionDecision tracks why a node was selected
type SelectionDecision struct {
	SelectedNode    string
	Reason          string // "height_winner", "round_robin", "only_available", "external_endpoint"
	Candidates      int
	MaxHeight       int64
	SelectedLatency time.Duration
}

// NewSelector creates a new node selector
func NewSelector(store *storage.HeightStore, endpointStore *storage.ExternalEndpointStore, configLoader *config.Loader, logger *zap.Logger) *Selector {
	return &Selector{
		store:         store,
		endpointStore: endpointStore,
		configLoader:  configLoader,
		logger:        logger,
	}
}

// SetHealthStore enables health-based filtering.
// When set, nodes whose protocol endpoint is unhealthy are excluded from selection.
func (s *Selector) SetHealthStore(hs *storage.HealthStore) {
	s.healthStore = hs
}

// SetArchivalFilter enables archival node filtering.
func (s *Selector) SetArchivalFilter(f *ArchivalFilter) {
	s.archivalFilter = f
}

// SetSyncFilter enables oracle drift filtering.
func (s *Selector) SetSyncFilter(f *SyncFilter) {
	s.syncFilter = f
}

// GetBestNode returns the best node for the given network and endpoint type
// The Eye sees all, the Dark Lord judges
func (s *Selector) GetBestNode(network, endpointType string) (*storage.NodeMetrics, string, *SelectionDecision) {
	// Get all internal nodes for this network and type
	nodesMap := s.store.GetByNetwork(network, endpointType)

	// Convert map to slice for easier processing.
	nodes := make([]nodeWithName, 0, len(nodesMap))
	for name, m := range nodesMap {
		nodes = append(nodes, nodeWithName{name: name, metrics: m})
	}

	s.logger.Debug("Selector: internal nodes retrieved",
		zap.String("network", network),
		zap.String("type", endpointType),
		zap.Int("count", len(nodes)),
	)

	// Filter by protocol health if HealthStore is available.
	if s.healthStore != nil {
		filtered := make([]nodeWithName, 0, len(nodes))
		for _, n := range nodes {
			if s.healthStore.IsHealthy(network, n.name, endpointType) {
				filtered = append(filtered, n)
			} else {
				s.logger.Debug("Selector: node excluded by health filter",
					zap.String("node", n.name),
					zap.String("protocol", endpointType),
				)
			}
		}
		nodes = filtered
	}

	// Filter by archival status if configured.
	nodes = s.archivalFilter.Filter(network, nodes)

	// Filter by sync/oracle drift if configured.
	nodes = s.syncFilter.Filter(network, nodes)

	// Find max internal height
	var maxInternalHeight int64
	for _, node := range nodes {
		if node.metrics.Height > maxInternalHeight {
			maxInternalHeight = node.metrics.Height
		}
	}

	// Get external endpoints and check if we should include them
	// Externals are added when: no healthy internals OR externals are ahead by threshold
	if s.endpointStore != nil {
		externalEndpoints := s.endpointStore.GetValidatedEndpoints(network, endpointType)

		// Get threshold from config (default to 2 blocks)
		cfg := s.configLoader.Get()
		threshold := cfg.ExternalFailoverThreshold
		if threshold == 0 {
			threshold = 2 // default threshold
		}

		// Find max external height
		var maxExternalHeight int64
		for _, ep := range externalEndpoints {
			if ep.Height > maxExternalHeight {
				maxExternalHeight = ep.Height
			}
		}

		// Add externals if: no healthy internals OR externals are significantly ahead
		shouldAddExternals := maxInternalHeight == 0 || maxExternalHeight > maxInternalHeight+threshold

		if shouldAddExternals && len(externalEndpoints) > 0 {
			s.logger.Debug("Selector: adding external endpoints to candidates",
				zap.String("network", network),
				zap.String("type", endpointType),
				zap.Int("external_count", len(externalEndpoints)),
				zap.Int64("max_internal_height", maxInternalHeight),
				zap.Int64("max_external_height", maxExternalHeight),
				zap.Int64("threshold", threshold),
			)

			for _, ep := range externalEndpoints {
				// Create a synthetic "node" entry for this external endpoint
				// Use URL as the identifier (prefixed with "ext:" to distinguish from internal nodes)
				nodeName := "ext:" + ep.URL
				nodeMetrics := &storage.NodeMetrics{
					Height:             ep.Height,
					AvgLatency:         ep.Latency,
					Timestamp:          ep.LastValidated,
					Source:             "external",
					WebSocketAvailable: ep.WebSocketAvailable,
				}
				nodes = append(nodes, nodeWithName{name: nodeName, metrics: nodeMetrics})

				s.logger.Debug("Selector: added external endpoint to candidates",
					zap.String("url", ep.URL),
					zap.Int64("height", ep.Height),
					zap.Duration("latency", ep.Latency),
				)
			}
		} else {
			s.logger.Debug("Selector: using internal nodes only",
				zap.String("network", network),
				zap.String("type", endpointType),
				zap.Int64("max_internal_height", maxInternalHeight),
				zap.Int64("max_external_height", maxExternalHeight),
				zap.Int64("threshold", threshold),
			)
		}
	}

	if len(nodes) == 0 {
		s.logger.Warn("No nodes available for routing",
			zap.String("network", network),
			zap.String("type", endpointType),
		)
		metrics.RoutingFailures.WithLabelValues(network, endpointType, "no_nodes").Inc()
		return nil, "", nil
	}

	s.logger.Debug("Selector: total candidates",
		zap.String("network", network),
		zap.String("type", endpointType),
		zap.Int("total", len(nodes)),
	)

	decision := &SelectionDecision{
		Candidates: len(nodes),
	}

	// Record alternatives considered
	metrics.RoutingAlternativesConsidered.WithLabelValues(network, endpointType).Observe(float64(len(nodes)))

	// Step 1: Find the maximum height
	var maxHeight int64
	for _, node := range nodes {
		if node.metrics.Height > maxHeight {
			maxHeight = node.metrics.Height
		}
		s.logger.Debug("Selector: candidate node",
			zap.String("node", node.name),
			zap.Int64("height", node.metrics.Height),
			zap.Duration("latency", node.metrics.AvgLatency),
			zap.String("source", node.metrics.Source),
		)
	}
	decision.MaxHeight = maxHeight

	s.logger.Debug("Selector: max height determined",
		zap.String("network", network),
		zap.String("type", endpointType),
		zap.Int64("max_height", maxHeight),
	)

	if maxHeight == 0 {
		s.logger.Warn("All nodes have zero height",
			zap.String("network", network),
			zap.String("type", endpointType),
			zap.Int("candidates", len(nodes)),
		)
		metrics.RoutingFailures.WithLabelValues(network, endpointType, "zero_height").Inc()
		return nil, "", nil
	}

	// Step 2: Filter nodes with maximum height
	maxHeightNodes := make([]nodeWithName, 0)
	for _, node := range nodes {
		if node.metrics.Height == maxHeight {
			maxHeightNodes = append(maxHeightNodes, node)
		}
	}

	// Step 2b: Sort maxHeightNodes by name for deterministic round-robin (M23)
	sort.Slice(maxHeightNodes, func(i, j int) bool {
		return maxHeightNodes[i].name < maxHeightNodes[j].name
	})

	// Step 3: Among nodes with max height, distribute using per-key round-robin (M24)
	rrKey := network + ":" + endpointType
	counterPtr, _ := s.rrCounters.LoadOrStore(rrKey, new(uint64))
	counter := atomic.AddUint64(counterPtr.(*uint64), 1)
	selectedIndex := int(counter % uint64(len(maxHeightNodes)))
	bestNode := maxHeightNodes[selectedIndex]

	// Determine selection reason
	if len(nodes) == 1 {
		decision.Reason = "only_available"
	} else if len(maxHeightNodes) == 1 {
		decision.Reason = "height_winner"
	} else {
		decision.Reason = "round_robin"
	}

	decision.SelectedNode = bestNode.name
	decision.SelectedLatency = bestNode.metrics.AvgLatency

	// Record metrics — use node source category to avoid cardinality explosion (H10)
	nodeSource := bestNode.metrics.Source
	if nodeSource == "" {
		nodeSource = "internal"
	}
	metrics.RoutingSelections.WithLabelValues(
		network,
		endpointType,
		nodeSource,
		decision.Reason,
	).Inc()

	s.logger.Debug("Node selected",
		zap.String("network", network),
		zap.String("type", endpointType),
		zap.String("selected_node", bestNode.name),
		zap.String("reason", decision.Reason),
		zap.Int("candidates", decision.Candidates),
		zap.Int64("height", maxHeight),
		zap.Duration("latency", bestNode.metrics.AvgLatency),
		zap.Int("max_height_nodes", len(maxHeightNodes)),
	)

	return bestNode.metrics, bestNode.name, decision
}

// GetEndpointURL returns the full endpoint URL for a node
func (s *Selector) GetEndpointURL(nodeName, endpointType string) string {
	cfg := s.configLoader.Get()

	// Search in internal nodes
	for _, node := range cfg.Internals {
		if node.Name == nodeName {
			switch endpointType {
			case "api":
				return urlutil.NormalizeURL(node.API)
			case "rpc":
				return urlutil.NormalizeURL(node.RPC)
			case "grpc":
				return node.GRPC // gRPC doesn't need normalization
			}
		}
	}

	// Check if it's an external endpoint (nodeName format: "ext:{url}")
	// External endpoints are identified by their URL stored in the node name
	if len(nodeName) > 4 && nodeName[:4] == "ext:" {
		url := nodeName[4:]
		return url
	}

	s.logger.Warn("Node not found in configuration",
		zap.String("node", nodeName),
		zap.String("type", endpointType),
	)

	return ""
}

// GetHighestHeights returns the highest height for each enabled endpoint type
// Used by the status API
func (s *Selector) GetHighestHeights(network string, enabledTypes []string) map[string]int64 {
	result := make(map[string]int64)

	for _, typ := range enabledTypes {
		// Get highest height from internal nodes
		height := s.store.GetHighestHeight(network, typ)

		// Also check external endpoints
		if s.endpointStore != nil {
			externalEndpoints := s.endpointStore.GetValidatedEndpoints(network, typ)
			for _, ep := range externalEndpoints {
				if ep.Height > height {
					height = ep.Height
				}
			}
		}

		if height > 0 {
			result[typ] = height
		}
	}

	return result
}
