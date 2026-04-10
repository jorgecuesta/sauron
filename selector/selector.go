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
	configLoader   *config.MultiChainLoader
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
func NewSelector(store *storage.HeightStore, endpointStore *storage.ExternalEndpointStore, configLoader *config.MultiChainLoader, logger *zap.Logger) *Selector {
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

// normalizeProtocol maps V1 endpoint type names to V2 protocol names.
// Temporary bridge until task #21 refactors proxies to use V2 naming.
func normalizeProtocol(endpointType string) string {
	if endpointType == "api" {
		return "rest"
	}
	return endpointType
}

// heightCheckProtocol returns the protocol used for height checks for a network.
// Falls back to "rpc" if the network or height check config is not found.
func (s *Selector) heightCheckProtocol(network string) string {
	cfg := s.configLoader.Get()
	net := cfg.FindNetwork(network)
	if net == nil {
		return "rpc"
	}
	// The height check protocol is determined by the adapter factory.
	// For native adapters (cosmos/evm/solana), the adapter chooses which
	// protocol to use for height checks. HeightCheckProtocol() returns
	// the explicit protocol from config, or "" for native adapters.
	protocol := net.HeightCheckProtocol()
	if protocol != "" {
		return protocol
	}

	// Native adapter defaults based on chain type.
	switch net.Type {
	case "cosmos":
		return "rpc"
	case "evm":
		return "jsonrpc"
	case "solana":
		return "jsonrpc"
	default:
		// For custom type, the height check protocol should be explicitly set.
		// Fall back to the first endpoint protocol.
		if len(net.Endpoints) > 0 {
			return net.Endpoints[0].Protocol
		}
		return "rpc"
	}
}

// GetBestNode returns the best node for the given network and endpoint type.
//
// V2 model: Heights are stored under the height-check protocol only.
// The selector queries heights by the network's height-check protocol,
// then filters by health for the requested endpoint type.
func (s *Selector) GetBestNode(network, endpointType string) (*storage.NodeMetrics, string, *SelectionDecision) {
	// Normalize V1 protocol names to V2.
	requestedProtocol := normalizeProtocol(endpointType)

	// Get the height-check protocol for this network.
	heightProtocol := s.heightCheckProtocol(network)

	// Get all internal nodes that have heights (via the height-check protocol).
	nodesMap := s.store.GetByNetwork(network, heightProtocol)

	// Convert map to slice for processing.
	nodes := make([]nodeWithName, 0, len(nodesMap))
	for name, m := range nodesMap {
		nodes = append(nodes, nodeWithName{name: name, metrics: m})
	}

	s.logger.Debug("Selector: internal nodes retrieved",
		zap.String("network", network),
		zap.String("requested_protocol", requestedProtocol),
		zap.String("height_protocol", heightProtocol),
		zap.Int("count", len(nodes)),
	)

	// Filter by protocol health for the REQUESTED protocol (not height-check protocol).
	// This ensures we only route to nodes where the requested endpoint is alive.
	if s.healthStore != nil {
		filtered := make([]nodeWithName, 0, len(nodes))
		for _, n := range nodes {
			if s.healthStore.IsHealthy(network, n.name, requestedProtocol) {
				filtered = append(filtered, n)
			} else {
				s.logger.Debug("Selector: node excluded by health filter",
					zap.String("node", n.name),
					zap.String("protocol", requestedProtocol),
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
	if s.endpointStore != nil {
		externalEndpoints := s.endpointStore.GetValidatedEndpoints(network, endpointType)

		cfg := s.configLoader.Get()
		threshold := cfg.ExternalFailoverThreshold
		if threshold == 0 {
			threshold = 2
		}

		var maxExternalHeight int64
		for _, ep := range externalEndpoints {
			if ep.Height > maxExternalHeight {
				maxExternalHeight = ep.Height
			}
		}

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
				nodeName := "ext:" + ep.URL
				nodeMetrics := &storage.NodeMetrics{
					Height:             ep.Height,
					AvgLatency:         ep.Latency,
					Timestamp:          ep.LastValidated,
					Source:             "external",
					WebSocketAvailable: ep.WebSocketAvailable,
				}
				nodes = append(nodes, nodeWithName{name: nodeName, metrics: nodeMetrics})
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

	decision := &SelectionDecision{
		Candidates: len(nodes),
	}

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

	// Step 2b: Sort by name for deterministic round-robin
	sort.Slice(maxHeightNodes, func(i, j int) bool {
		return maxHeightNodes[i].name < maxHeightNodes[j].name
	})

	// Step 3: Round-robin among max-height nodes
	rrKey := network + ":" + endpointType
	counterPtr, _ := s.rrCounters.LoadOrStore(rrKey, new(uint64))
	counter := atomic.AddUint64(counterPtr.(*uint64), 1)
	selectedIndex := int(counter % uint64(len(maxHeightNodes)))
	bestNode := maxHeightNodes[selectedIndex]

	if len(nodes) == 1 {
		decision.Reason = "only_available"
	} else if len(maxHeightNodes) == 1 {
		decision.Reason = "height_winner"
	} else {
		decision.Reason = "round_robin"
	}

	decision.SelectedNode = bestNode.name
	decision.SelectedLatency = bestNode.metrics.AvgLatency

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

// GetEndpointURL returns the full endpoint URL for a node.
// Uses V2 MultiChainNode.Endpoints map for URL lookup.
func (s *Selector) GetEndpointURL(nodeName, endpointType string) string {
	cfg := s.configLoader.Get()

	// Normalize V1 protocol names to V2.
	protocol := normalizeProtocol(endpointType)

	// Search in internal nodes
	for _, node := range cfg.Internals {
		if node.Name == nodeName {
			url := node.Endpoints[protocol]
			if url == "" {
				return ""
			}
			if protocol == "grpc" {
				return url // gRPC doesn't need normalization
			}
			return urlutil.NormalizeURL(url)
		}
	}

	// Check if it's an external endpoint (nodeName format: "ext:{url}")
	if len(nodeName) > 4 && nodeName[:4] == "ext:" {
		return nodeName[4:]
	}

	s.logger.Warn("Node not found in configuration",
		zap.String("node", nodeName),
		zap.String("type", endpointType),
	)

	return ""
}

// GetHighestHeights returns the highest height for each enabled endpoint type.
// In V2, all protocols share the same height (from the height-check protocol),
// so the height is the same across all protocols for a network.
func (s *Selector) GetHighestHeights(network string, enabledTypes []string) map[string]int64 {
	result := make(map[string]int64)

	// Get the height-check protocol for this network.
	heightProtocol := s.heightCheckProtocol(network)

	// Get highest height from internal nodes (via height-check protocol).
	height := s.store.GetHighestHeight(network, heightProtocol)

	// Also check external endpoints for each type.
	for _, typ := range enabledTypes {
		h := height

		if s.endpointStore != nil {
			externalEndpoints := s.endpointStore.GetValidatedEndpoints(network, typ)
			for _, ep := range externalEndpoints {
				if ep.Height > h {
					h = ep.Height
				}
			}
		}

		if h > 0 {
			result[typ] = h
		}
	}

	return result
}
