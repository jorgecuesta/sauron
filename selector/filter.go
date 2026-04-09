package selector

import (
	"sync"

	"sauron/storage"

	"go.uber.org/zap"
)

// ArchivalFilter excludes nodes that are not archival when archival routing
// is required for the network. A node is considered archival if it has been
// checked and confirmed to have blocks at the configured min_height.
//
// The filter is only applied for networks that have been registered as
// requiring archival data via RequireArchival. Networks without archival
// requirements pass through unfiltered.
type ArchivalFilter struct {
	archivalNodes    *storage.ArchivalStore
	archivalNetworks sync.Map // network → bool, networks that require archival
	logger           *zap.Logger
}

// NewArchivalFilter creates a new archival filter.
func NewArchivalFilter(archivalNodes *storage.ArchivalStore, logger *zap.Logger) *ArchivalFilter {
	return &ArchivalFilter{
		archivalNodes: archivalNodes,
		logger:        logger,
	}
}

// RequireArchival marks a network as requiring archival data from nodes.
// Only networks registered here will have archival filtering applied.
func (f *ArchivalFilter) RequireArchival(network string) {
	if f == nil {
		return
	}
	f.archivalNetworks.Store(network, true)
}

// Filter removes non-archival nodes from the candidate list.
// Returns the original list unchanged if:
//   - the filter is nil
//   - the archival store is nil
//   - the network does not require archival data
func (f *ArchivalFilter) Filter(network string, nodes []nodeWithName) []nodeWithName {
	if f == nil || f.archivalNodes == nil {
		return nodes
	}

	// Only filter if this network requires archival.
	if _, required := f.archivalNetworks.Load(network); !required {
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
//
// Max drift is configured per-network via SetMaxDrift. Networks without
// a configured max drift pass through unfiltered.
type SyncFilter struct {
	oracleStore *storage.OracleStore
	maxDrifts   sync.Map // network → int64
	logger      *zap.Logger
}

// NewSyncFilter creates a new sync drift filter.
func NewSyncFilter(oracleStore *storage.OracleStore, logger *zap.Logger) *SyncFilter {
	return &SyncFilter{
		oracleStore: oracleStore,
		logger:      logger,
	}
}

// SetMaxDrift configures the max drift for a specific network.
func (f *SyncFilter) SetMaxDrift(network string, maxDrift int64) {
	if f == nil {
		return
	}
	f.maxDrifts.Store(network, maxDrift)
}

// getMaxDrift returns the configured max drift for a network.
// Returns 0 if not configured (no filtering).
func (f *SyncFilter) getMaxDrift(network string) int64 {
	v, ok := f.maxDrifts.Load(network)
	if !ok {
		return 0
	}
	return v.(int64)
}

// Filter removes nodes that have drifted beyond the configured maxDrift
// from the oracle height. Returns the original list unchanged if:
//   - the filter is nil
//   - the oracle store is nil
//   - no max drift is configured for this network
//   - no oracle height exists for this network
func (f *SyncFilter) Filter(network string, nodes []nodeWithName) []nodeWithName {
	if f == nil || f.oracleStore == nil {
		return nodes
	}

	maxDrift := f.getMaxDrift(network)
	if maxDrift <= 0 {
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
