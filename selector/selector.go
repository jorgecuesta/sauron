package selector

import (
	"math"
	"time"

	"sauron/config"
	"sauron/metrics"
	"sauron/storage"

	"go.uber.org/zap"
)

// Selector chooses the best node for a given network and endpoint type
// The Dark Lord's judgment - highest height → lowest latency
type Selector struct {
	store         *storage.HeightStore
	endpointStore *storage.ExternalEndpointStore
	configLoader  *config.Loader
	logger        *zap.Logger
}

// SelectionDecision tracks why a node was selected
type SelectionDecision struct {
	SelectedNode    string
	Reason          string // "height_winner", "latency_tiebreaker", "only_available", "external_endpoint"
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

// GetBestNode returns the best node for the given network and endpoint type
// The Eye sees all, the Dark Lord judges
func (s *Selector) GetBestNode(network, endpointType string) (*storage.NodeMetrics, string, *SelectionDecision) {
	// Get all internal nodes for this network and type
	nodesMap := s.store.GetByNetwork(network, endpointType)

	// Convert map to slice for easier processing
	type nodeWithName struct {
		name    string
		metrics *storage.NodeMetrics
	}

	nodes := make([]nodeWithName, 0, len(nodesMap))
	for name, m := range nodesMap {
		nodes = append(nodes, nodeWithName{name: name, metrics: m})
	}

	s.logger.Debug("Selector: internal nodes retrieved",
		zap.String("network", network),
		zap.String("type", endpointType),
		zap.Int("count", len(nodes)),
	)

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
			s.logger.Info("Selector: adding external endpoints to candidates",
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

	s.logger.Info("Selector: total candidates",
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
		s.logger.Info("Selector: candidate node",
			zap.String("node", node.name),
			zap.Int64("height", node.metrics.Height),
			zap.Duration("latency", node.metrics.AvgLatency),
			zap.String("source", node.metrics.Source),
		)
	}
	decision.MaxHeight = maxHeight

	s.logger.Info("Selector: max height determined",
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

	// Step 3: Among nodes with max height, select the one with lowest latency
	var bestNode nodeWithName
	minLatency := time.Duration(math.MaxInt64)

	for _, node := range maxHeightNodes {
		if node.metrics.AvgLatency < minLatency {
			minLatency = node.metrics.AvgLatency
			bestNode = node
		}
	}

	// Determine selection reason
	if len(nodes) == 1 {
		decision.Reason = "only_available"
	} else if len(maxHeightNodes) == 1 {
		decision.Reason = "height_winner"
	} else {
		decision.Reason = "latency_tiebreaker"
	}

	decision.SelectedNode = bestNode.name
	decision.SelectedLatency = bestNode.metrics.AvgLatency

	// Record metrics
	metrics.RoutingSelections.WithLabelValues(
		network,
		endpointType,
		bestNode.name,
		decision.Reason,
	).Inc()

	s.logger.Debug("Node selected",
		zap.String("network", network),
		zap.String("type", endpointType),
		zap.String("selected_node", bestNode.name),
		zap.String("reason", decision.Reason),
		zap.Int("candidates", decision.Candidates),
		zap.Int64("height", maxHeight),
		zap.Duration("latency", minLatency),
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
				return normalizeURL(node.API)
			case "rpc":
				return normalizeURL(node.RPC)
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

// normalizeURL ensures URL has proper scheme
func normalizeURL(url string) string {
	if url == "" {
		return ""
	}
	if len(url) > 0 && url[0] != 'h' {
		return "https://" + url
	}
	return url
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
