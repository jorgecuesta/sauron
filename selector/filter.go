package selector

import (
	"sauron/storage"

	"go.uber.org/zap"
)

// ArchivalFilter excludes nodes that are not archival when archival routing
// is required for the network. A node is considered archival if it has been
// checked and confirmed to have blocks at the configured min_height.
//
// archivalNodes is a set of node names known to have archival data.
// If archivalNodes is nil, the filter is disabled (no filtering).
type ArchivalFilter struct {
	// ArchivalNodes tracks which nodes have archival data: network → set of node names.
	// Populated by the checker when it verifies a node has blocks at min_height.
	archivalNodes *storage.ArchivalStore
	logger        *zap.Logger
}

// NewArchivalFilter creates a new archival filter.
func NewArchivalFilter(archivalNodes *storage.ArchivalStore, logger *zap.Logger) *ArchivalFilter {
	return &ArchivalFilter{
		archivalNodes: archivalNodes,
		logger:        logger,
	}
}

// Filter removes non-archival nodes from the candidate list.
// Returns the filtered list. If the archival store is nil or the network
// has no archival requirement, returns the original list unchanged.
func (f *ArchivalFilter) Filter(network string, nodes []nodeWithName) []nodeWithName {
	if f == nil || f.archivalNodes == nil {
		return nodes
	}

	filtered := make([]nodeWithName, 0, len(nodes))
	for _, n := range nodes {
		if f.archivalNodes.IsArchival(network, n.name) {
			filtered = append(filtered, n)
		} else {
			f.logger.Debug("Selector: node excluded by archival filter",
				zap.String("node", n.name),
				zap.String("network", network),
			)
		}
	}
	return filtered
}

// SyncFilter excludes nodes that have drifted too far from the oracle
// reference height. A node is excluded if:
//
//	|oracle_height - node_height| > maxDrift
//
// This catches both nodes that are behind (lagging) and nodes that
// report implausibly high heights (potential fork or error).
type SyncFilter struct {
	oracleStore *storage.OracleStore
	logger      *zap.Logger
}

// NewSyncFilter creates a new sync drift filter.
func NewSyncFilter(oracleStore *storage.OracleStore, logger *zap.Logger) *SyncFilter {
	return &SyncFilter{
		oracleStore: oracleStore,
		logger:      logger,
	}
}

// Filter removes nodes that have drifted beyond maxDrift from the oracle height.
// If maxDrift is 0 or negative, or no oracle height exists for the network,
// returns the original list unchanged.
func (f *SyncFilter) Filter(network string, maxDrift int64, nodes []nodeWithName) []nodeWithName {
	if f == nil || f.oracleStore == nil || maxDrift <= 0 {
		return nodes
	}

	oracleHeight, ok := f.oracleStore.Get(network)
	if !ok {
		// No oracle data yet — can't filter.
		return nodes
	}

	filtered := make([]nodeWithName, 0, len(nodes))
	for _, n := range nodes {
		drift := oracleHeight - n.metrics.Height
		if drift < 0 {
			drift = -drift
		}

		if drift <= maxDrift {
			filtered = append(filtered, n)
		} else {
			f.logger.Debug("Selector: node excluded by sync filter",
				zap.String("node", n.name),
				zap.String("network", network),
				zap.Int64("node_height", n.metrics.Height),
				zap.Int64("oracle_height", oracleHeight),
				zap.Int64("drift", drift),
				zap.Int64("max_drift", maxDrift),
			)
		}
	}
	return filtered
}
