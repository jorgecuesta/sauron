package selector

import (
	"testing"
	"time"

	"sauron/storage"

	"go.uber.org/zap"
)

// setupFullPipeline creates a Selector with all filters wired,
// matching the server.New() wiring. This is the closest we can get
// to testing the real production configuration without starting HTTP servers.
func setupFullPipeline(t *testing.T) (
	*Selector,
	*storage.HeightStore,
	*storage.HealthStore,
	*storage.ArchivalStore,
	*storage.OracleStore,
	*ArchivalFilter,
	*SyncFilter,
) {
	t.Helper()

	heightStore := storage.NewHeightStore()
	healthStore := storage.NewHealthStore()
	archivalStore := storage.NewArchivalStore()
	oracleStore := storage.NewOracleStore()
	configLoader := createTestConfig(t, 2)
	logger := zap.NewNop()

	sel := NewSelector(heightStore, nil, configLoader, logger)

	// Wire exactly as server.New() does.
	sel.SetHealthStore(healthStore)
	archFilter := NewArchivalFilter(archivalStore, logger)
	syncFilter := NewSyncFilter(oracleStore, logger)
	sel.SetArchivalFilter(archFilter)
	sel.SetSyncFilter(syncFilter)

	return sel, heightStore, healthStore, archivalStore, oracleStore, archFilter, syncFilter
}

// TestPipeline_AllFiltersActive tests the full pipeline with all three
// filters active simultaneously. This is the primary integration test.
func TestPipeline_AllFiltersActive(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore, archivalStore, oracleStore, archFilter, syncFilter := setupFullPipeline(t)

	// Configure network for archival + sync.
	archFilter.RequireArchival("pocket")
	syncFilter.SetMaxDrift("pocket", 5)
	oracleStore.Update("pocket", 100, "oracle")

	// 4 nodes, each fails a different filter:
	//   node-1: healthy=yes, archival=yes, drift=0  → PASSES ALL
	//   node-2: healthy=NO,  archival=yes, drift=0  → fails health
	//   node-3: healthy=yes, archival=NO,  drift=0  → fails archival
	//   node-4: healthy=yes, archival=yes, drift=20 → fails sync
	heightStore.Update("pocket", "node-1", "api", 100, 10*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "api", 100, 10*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-3", "api", 100, 10*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-4", "api", 80, 10*time.Millisecond, "internal")

	healthStore.SetHealthy("pocket", "node-1", "api")
	healthStore.SetUnhealthy("pocket", "node-2", "api", "connection refused")
	healthStore.SetHealthy("pocket", "node-3", "api")
	healthStore.SetHealthy("pocket", "node-4", "api")

	archivalStore.SetArchival("pocket", "node-1")
	archivalStore.SetArchival("pocket", "node-2")
	archivalStore.SetNotArchival("pocket", "node-3")
	archivalStore.SetArchival("pocket", "node-4")

	m, nodeName, decision := sel.GetBestNode("pocket", "api")

	if m == nil {
		t.Fatal("expected a node to be selected")
	}
	if nodeName != "node-1" {
		t.Fatalf("expected node-1 (only node passing all filters), got %s", nodeName)
	}
	if decision.Candidates != 1 {
		t.Fatalf("expected 1 candidate, got %d", decision.Candidates)
	}
}

// TestPipeline_FiltersWiredButUnconfigured tests that filters wired in
// server.New() but without per-network config (no RequireArchival, no
// SetMaxDrift) do NOT filter anything. This is the V1 compatibility path.
func TestPipeline_FiltersWiredButUnconfigured(t *testing.T) {
	t.Parallel()

	sel, heightStore, _, _, _, _, _ := setupFullPipeline(t)

	// No RequireArchival, no SetMaxDrift — filters should be inert.
	// But HealthStore IS wired, so health filter is active.
	// Since no health data exists, all nodes will be excluded by health filter.
	// This is the expected conservative behavior.

	heightStore.Update("pocket", "node-1", "api", 100, 10*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "api", 100, 10*time.Millisecond, "internal")

	m, _, _ := sel.GetBestNode("pocket", "api")

	// HealthStore wired but no health data → all nodes excluded (conservative).
	if m != nil {
		t.Fatal("expected nil: HealthStore wired but no health data should exclude all nodes")
	}
}

// TestPipeline_HealthyButNoArchivalOrSyncConfig tests nodes pass through
// archival and sync filters when those features aren't configured for the network.
func TestPipeline_HealthyButNoArchivalOrSyncConfig(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore, _, _, _, _ := setupFullPipeline(t)

	// Only health data — no archival/sync config.
	heightStore.Update("pocket", "node-1", "api", 100, 10*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "api", 99, 10*time.Millisecond, "internal")

	healthStore.SetHealthy("pocket", "node-1", "api")
	healthStore.SetHealthy("pocket", "node-2", "api")

	m, nodeName, decision := sel.GetBestNode("pocket", "api")

	if m == nil {
		t.Fatal("expected node to be selected")
	}
	// node-1 has higher height, should be selected.
	if nodeName != "node-1" {
		t.Fatalf("expected node-1 (higher height), got %s", nodeName)
	}
	// Candidates counts total nodes after filtering (both survived), not just max-height.
	if decision.Candidates != 2 {
		t.Fatalf("expected 2 candidates (both healthy), got %d", decision.Candidates)
	}
}

// TestPipeline_FilterOrder verifies filters execute in the correct order:
// health → archival → sync. A node that would pass sync but fail health
// should be excluded by health, not reach sync.
func TestPipeline_FilterOrder(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore, archivalStore, oracleStore, archFilter, syncFilter := setupFullPipeline(t)

	archFilter.RequireArchival("pocket")
	syncFilter.SetMaxDrift("pocket", 5)
	oracleStore.Update("pocket", 100, "oracle")

	// node-1: unhealthy but passes archival+sync.
	// If filter order is wrong, it might reach archival/sync.
	heightStore.Update("pocket", "node-1", "api", 100, 10*time.Millisecond, "internal")
	healthStore.SetUnhealthy("pocket", "node-1", "api", "down")
	archivalStore.SetArchival("pocket", "node-1")

	m, _, _ := sel.GetBestNode("pocket", "api")

	if m != nil {
		t.Fatal("unhealthy node should be excluded regardless of archival/sync status")
	}
}

// TestPipeline_HealthyNodeFailsArchival tests that a healthy node is still
// excluded if it fails the archival check.
func TestPipeline_HealthyNodeFailsArchival(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore, archivalStore, _, archFilter, _ := setupFullPipeline(t)

	archFilter.RequireArchival("pocket")

	heightStore.Update("pocket", "node-1", "api", 100, 10*time.Millisecond, "internal")
	healthStore.SetHealthy("pocket", "node-1", "api")
	archivalStore.SetNotArchival("pocket", "node-1")

	m, _, _ := sel.GetBestNode("pocket", "api")

	if m != nil {
		t.Fatal("healthy but non-archival node should be excluded when archival is required")
	}
}

// TestPipeline_ArchivalPassesSyncFails tests that a node passing health
// and archival is still excluded if it fails the sync drift check.
func TestPipeline_ArchivalPassesSyncFails(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore, archivalStore, oracleStore, archFilter, syncFilter := setupFullPipeline(t)

	archFilter.RequireArchival("pocket")
	syncFilter.SetMaxDrift("pocket", 5)
	oracleStore.Update("pocket", 100, "oracle")

	heightStore.Update("pocket", "node-1", "api", 80, 10*time.Millisecond, "internal") // drift=20
	healthStore.SetHealthy("pocket", "node-1", "api")
	archivalStore.SetArchival("pocket", "node-1")

	m, _, _ := sel.GetBestNode("pocket", "api")

	if m != nil {
		t.Fatal("node passing health+archival but failing sync should be excluded")
	}
}

// TestPipeline_MultipleNetworksIndependent tests that filter configurations
// for one network don't affect another.
func TestPipeline_MultipleNetworksIndependent(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore, archivalStore, oracleStore, archFilter, syncFilter := setupFullPipeline(t)

	// "pocket" has archival+sync requirements.
	archFilter.RequireArchival("pocket")
	syncFilter.SetMaxDrift("pocket", 5)
	oracleStore.Update("pocket", 100, "oracle-pocket")

	// "ethereum" has NO archival/sync requirements — just health.

	// pocket node: passes health, fails archival.
	heightStore.Update("pocket", "node-p", "api", 100, 10*time.Millisecond, "internal")
	healthStore.SetHealthy("pocket", "node-p", "api")
	archivalStore.SetNotArchival("pocket", "node-p")

	// ethereum node: passes health, no archival/sync needed.
	heightStore.Update("ethereum", "node-e", "api", 20000000, 10*time.Millisecond, "internal")
	healthStore.SetHealthy("ethereum", "node-e", "api")

	// pocket should fail (archival required, node not archival).
	m, _, _ := sel.GetBestNode("pocket", "api")
	if m != nil {
		t.Fatal("pocket: expected nil (fails archival)")
	}

	// ethereum should succeed (no archival/sync config).
	m, nodeName, _ := sel.GetBestNode("ethereum", "api")
	if m == nil {
		t.Fatal("ethereum: expected node to be selected (no filters configured)")
	}
	if nodeName != "node-e" {
		t.Fatalf("ethereum: expected node-e, got %s", nodeName)
	}
}

// TestPipeline_NodeRecoveryThroughAllFilters tests that a node that was
// previously excluded by all filters can recover and be selected again.
func TestPipeline_NodeRecoveryThroughAllFilters(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore, archivalStore, oracleStore, archFilter, syncFilter := setupFullPipeline(t)

	archFilter.RequireArchival("pocket")
	syncFilter.SetMaxDrift("pocket", 5)
	oracleStore.Update("pocket", 100, "oracle")

	heightStore.Update("pocket", "node-1", "api", 100, 10*time.Millisecond, "internal")

	// Start: fails all three.
	healthStore.SetUnhealthy("pocket", "node-1", "api", "down")
	archivalStore.SetNotArchival("pocket", "node-1")

	m, _, _ := sel.GetBestNode("pocket", "api")
	if m != nil {
		t.Fatal("step 1: expected nil (all filters fail)")
	}

	// Step 2: health recovers, still fails archival.
	healthStore.SetHealthy("pocket", "node-1", "api")
	m, _, _ = sel.GetBestNode("pocket", "api")
	if m != nil {
		t.Fatal("step 2: expected nil (still fails archival)")
	}

	// Step 3: archival confirmed — now passes all filters.
	archivalStore.SetArchival("pocket", "node-1")
	m, nodeName, _ := sel.GetBestNode("pocket", "api")
	if m == nil {
		t.Fatal("step 3: expected node after full recovery")
	}
	if nodeName != "node-1" {
		t.Fatalf("step 3: expected node-1, got %s", nodeName)
	}
}

// TestPipeline_RoundRobinAfterFiltering tests that round-robin still works
// correctly after filters reduce the candidate set.
func TestPipeline_RoundRobinAfterFiltering(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore, _, _, _, _ := setupFullPipeline(t)

	// 3 nodes at same height, only 2 healthy.
	heightStore.Update("pocket", "node-a", "rpc", 100, 10*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-b", "rpc", 100, 10*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-c", "rpc", 100, 10*time.Millisecond, "internal")

	healthStore.SetHealthy("pocket", "node-a", "rpc")
	healthStore.SetHealthy("pocket", "node-b", "rpc")
	healthStore.SetUnhealthy("pocket", "node-c", "rpc", "down")

	// Make 10 selections — should round-robin between node-a and node-b only.
	seen := make(map[string]int)
	for i := 0; i < 10; i++ {
		_, nodeName, _ := sel.GetBestNode("pocket", "rpc")
		seen[nodeName]++
	}

	if len(seen) != 2 {
		t.Fatalf("expected 2 unique nodes in round-robin, got %d: %v", len(seen), seen)
	}
	if _, ok := seen["node-c"]; ok {
		t.Fatal("unhealthy node-c should never be selected")
	}
	if seen["node-a"] == 0 || seen["node-b"] == 0 {
		t.Fatalf("expected both healthy nodes selected, got: %v", seen)
	}
}

// TestPipeline_ZeroHeightAfterFiltering tests that if all remaining nodes
// after filtering have zero height, no node is selected.
func TestPipeline_ZeroHeightAfterFiltering(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore, _, _, _, _ := setupFullPipeline(t)

	// Node with height 0 (checker hasn't run yet).
	heightStore.Update("pocket", "node-1", "api", 0, 10*time.Millisecond, "internal")
	healthStore.SetHealthy("pocket", "node-1", "api")

	m, _, _ := sel.GetBestNode("pocket", "api")
	if m != nil {
		t.Fatal("expected nil: node with zero height should not be selected")
	}
}

// --- Filter combination matrix tests ---
// Tests every combination of the 3 optional filters (Health, Archival, Sync).
// setupFullPipeline always wires all filters, but they are only active
// when configured (SetHealthStore, RequireArchival, SetMaxDrift).

// TestPipeline_NoFilters_V1Pure tests with NO filters active at all.
// This is V1 behavior — raw height selection with no health/archival/sync.
func TestPipeline_NoFilters_V1Pure(t *testing.T) {
	t.Parallel()

	configLoader := createTestConfig(t, 2)
	heightStore := storage.NewHeightStore()
	logger := zap.NewNop()

	// No HealthStore, no ArchivalFilter, no SyncFilter.
	sel := NewSelector(heightStore, nil, configLoader, logger)

	heightStore.Update("pocket", "node-1", "api", 100, 10*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "api", 99, 10*time.Millisecond, "internal")

	m, nodeName, _ := sel.GetBestNode("pocket", "api")
	if m == nil {
		t.Fatal("expected node selected with no filters")
	}
	if nodeName != "node-1" {
		t.Fatalf("expected node-1 (highest), got %s", nodeName)
	}
}

// TestPipeline_OnlyArchival tests with only archival filter active.
func TestPipeline_OnlyArchival(t *testing.T) {
	t.Parallel()

	configLoader := createTestConfig(t, 2)
	heightStore := storage.NewHeightStore()
	archivalStore := storage.NewArchivalStore()
	logger := zap.NewNop()

	sel := NewSelector(heightStore, nil, configLoader, logger)
	archFilter := NewArchivalFilter(archivalStore, logger)
	archFilter.RequireArchival("pocket")
	sel.SetArchivalFilter(archFilter)
	// No HealthStore, no SyncFilter.

	heightStore.Update("pocket", "node-1", "api", 100, 10*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "api", 100, 10*time.Millisecond, "internal")

	archivalStore.SetArchival("pocket", "node-1")
	archivalStore.SetNotArchival("pocket", "node-2")

	m, nodeName, _ := sel.GetBestNode("pocket", "api")
	if m == nil {
		t.Fatal("expected node selected")
	}
	if nodeName != "node-1" {
		t.Fatalf("expected node-1 (archival), got %s", nodeName)
	}
}

// TestPipeline_OnlySync tests with only sync filter active.
func TestPipeline_OnlySync(t *testing.T) {
	t.Parallel()

	configLoader := createTestConfig(t, 2)
	heightStore := storage.NewHeightStore()
	oracleStore := storage.NewOracleStore()
	logger := zap.NewNop()

	sel := NewSelector(heightStore, nil, configLoader, logger)
	syncFilter := NewSyncFilter(oracleStore, logger)
	syncFilter.SetMaxDrift("pocket", 5)
	sel.SetSyncFilter(syncFilter)
	// No HealthStore, no ArchivalFilter.

	oracleStore.Update("pocket", 100, "oracle")

	heightStore.Update("pocket", "node-1", "api", 100, 10*time.Millisecond, "internal") // drift=0
	heightStore.Update("pocket", "node-2", "api", 80, 10*time.Millisecond, "internal")  // drift=20

	m, nodeName, _ := sel.GetBestNode("pocket", "api")
	if m == nil {
		t.Fatal("expected node selected")
	}
	if nodeName != "node-1" {
		t.Fatalf("expected node-1 (within drift), got %s", nodeName)
	}
}

// TestPipeline_HealthAndArchival tests health + archival without sync.
func TestPipeline_HealthAndArchival(t *testing.T) {
	t.Parallel()

	configLoader := createTestConfig(t, 2)
	heightStore := storage.NewHeightStore()
	healthStore := storage.NewHealthStore()
	archivalStore := storage.NewArchivalStore()
	logger := zap.NewNop()

	sel := NewSelector(heightStore, nil, configLoader, logger)
	sel.SetHealthStore(healthStore)
	archFilter := NewArchivalFilter(archivalStore, logger)
	archFilter.RequireArchival("pocket")
	sel.SetArchivalFilter(archFilter)
	// No SyncFilter.

	heightStore.Update("pocket", "node-1", "api", 100, 10*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "api", 100, 10*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-3", "api", 100, 10*time.Millisecond, "internal")

	healthStore.SetHealthy("pocket", "node-1", "api")
	healthStore.SetHealthy("pocket", "node-2", "api")
	healthStore.SetUnhealthy("pocket", "node-3", "api", "down") // fails health

	archivalStore.SetArchival("pocket", "node-1")
	archivalStore.SetNotArchival("pocket", "node-2") // fails archival
	archivalStore.SetArchival("pocket", "node-3")

	m, nodeName, _ := sel.GetBestNode("pocket", "api")
	if m == nil {
		t.Fatal("expected node selected")
	}
	// node-1: healthy + archival ✓
	// node-2: healthy but not archival ✗
	// node-3: unhealthy ✗
	if nodeName != "node-1" {
		t.Fatalf("expected node-1, got %s", nodeName)
	}
}

// TestPipeline_HealthAndSync tests health + sync without archival.
func TestPipeline_HealthAndSync(t *testing.T) {
	t.Parallel()

	configLoader := createTestConfig(t, 2)
	heightStore := storage.NewHeightStore()
	healthStore := storage.NewHealthStore()
	oracleStore := storage.NewOracleStore()
	logger := zap.NewNop()

	sel := NewSelector(heightStore, nil, configLoader, logger)
	sel.SetHealthStore(healthStore)
	syncFilter := NewSyncFilter(oracleStore, logger)
	syncFilter.SetMaxDrift("pocket", 5)
	sel.SetSyncFilter(syncFilter)
	// No ArchivalFilter.

	oracleStore.Update("pocket", 100, "oracle")

	heightStore.Update("pocket", "node-1", "api", 100, 10*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "api", 80, 10*time.Millisecond, "internal") // drift=20

	healthStore.SetHealthy("pocket", "node-1", "api")
	healthStore.SetHealthy("pocket", "node-2", "api")

	m, nodeName, _ := sel.GetBestNode("pocket", "api")
	if m == nil {
		t.Fatal("expected node selected")
	}
	if nodeName != "node-1" {
		t.Fatalf("expected node-1 (within drift), got %s", nodeName)
	}
}

// TestPipeline_ArchivalAndSync tests archival + sync without health.
func TestPipeline_ArchivalAndSync(t *testing.T) {
	t.Parallel()

	configLoader := createTestConfig(t, 2)
	heightStore := storage.NewHeightStore()
	archivalStore := storage.NewArchivalStore()
	oracleStore := storage.NewOracleStore()
	logger := zap.NewNop()

	sel := NewSelector(heightStore, nil, configLoader, logger)
	archFilter := NewArchivalFilter(archivalStore, logger)
	archFilter.RequireArchival("pocket")
	sel.SetArchivalFilter(archFilter)
	syncFilter := NewSyncFilter(oracleStore, logger)
	syncFilter.SetMaxDrift("pocket", 5)
	sel.SetSyncFilter(syncFilter)
	// No HealthStore.

	oracleStore.Update("pocket", 100, "oracle")

	heightStore.Update("pocket", "node-1", "api", 100, 10*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "api", 80, 10*time.Millisecond, "internal") // drift=20
	heightStore.Update("pocket", "node-3", "api", 100, 10*time.Millisecond, "internal")

	archivalStore.SetArchival("pocket", "node-1")
	archivalStore.SetArchival("pocket", "node-2")
	archivalStore.SetNotArchival("pocket", "node-3") // fails archival

	m, nodeName, _ := sel.GetBestNode("pocket", "api")
	if m == nil {
		t.Fatal("expected node selected")
	}
	// node-1: archival + drift=0 ✓
	// node-2: archival but drift=20 ✗
	// node-3: not archival ✗
	if nodeName != "node-1" {
		t.Fatalf("expected node-1, got %s", nodeName)
	}
}
