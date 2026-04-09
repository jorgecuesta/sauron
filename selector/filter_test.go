package selector

import (
	"testing"
	"time"

	"sauron/storage"

	"go.uber.org/zap"
)

// --- Archival Filter Tests ---

func TestArchivalFilter_ExcludesNonArchival(t *testing.T) {
	t.Parallel()

	archStore := storage.NewArchivalStore()
	archStore.SetArchival("pocket", "seed-one")
	archStore.SetNotArchival("pocket", "seed-two")

	f := NewArchivalFilter(archStore, zap.NewNop())
	nodes := []nodeWithName{
		{name: "seed-one", metrics: &storage.NodeMetrics{Height: 100}},
		{name: "seed-two", metrics: &storage.NodeMetrics{Height: 100}},
	}

	result := f.Filter("pocket", nodes)
	if len(result) != 1 {
		t.Fatalf("expected 1 node, got %d", len(result))
	}
	if result[0].name != "seed-one" {
		t.Fatalf("expected seed-one, got %s", result[0].name)
	}
}

func TestArchivalFilter_AllArchival(t *testing.T) {
	t.Parallel()

	archStore := storage.NewArchivalStore()
	archStore.SetArchival("pocket", "seed-one")
	archStore.SetArchival("pocket", "seed-two")

	f := NewArchivalFilter(archStore, zap.NewNop())
	nodes := []nodeWithName{
		{name: "seed-one", metrics: &storage.NodeMetrics{Height: 100}},
		{name: "seed-two", metrics: &storage.NodeMetrics{Height: 100}},
	}

	result := f.Filter("pocket", nodes)
	if len(result) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(result))
	}
}

func TestArchivalFilter_NoneArchival(t *testing.T) {
	t.Parallel()

	archStore := storage.NewArchivalStore()
	archStore.SetNotArchival("pocket", "seed-one")
	archStore.SetNotArchival("pocket", "seed-two")

	f := NewArchivalFilter(archStore, zap.NewNop())
	nodes := []nodeWithName{
		{name: "seed-one", metrics: &storage.NodeMetrics{Height: 100}},
		{name: "seed-two", metrics: &storage.NodeMetrics{Height: 100}},
	}

	result := f.Filter("pocket", nodes)
	if len(result) != 0 {
		t.Fatalf("expected 0 nodes, got %d", len(result))
	}
}

func TestArchivalFilter_NeverCheckedExcluded(t *testing.T) {
	t.Parallel()

	archStore := storage.NewArchivalStore()
	// seed-one never checked — should be excluded (conservative).

	f := NewArchivalFilter(archStore, zap.NewNop())
	nodes := []nodeWithName{
		{name: "seed-one", metrics: &storage.NodeMetrics{Height: 100}},
	}

	result := f.Filter("pocket", nodes)
	if len(result) != 0 {
		t.Fatalf("expected 0 nodes (never checked), got %d", len(result))
	}
}

func TestArchivalFilter_NilFilter(t *testing.T) {
	t.Parallel()

	var f *ArchivalFilter
	nodes := []nodeWithName{
		{name: "seed-one", metrics: &storage.NodeMetrics{Height: 100}},
	}

	result := f.Filter("pocket", nodes)
	if len(result) != 1 {
		t.Fatalf("nil filter should pass through, got %d nodes", len(result))
	}
}

func TestArchivalFilter_NilStore(t *testing.T) {
	t.Parallel()

	f := NewArchivalFilter(nil, zap.NewNop())
	nodes := []nodeWithName{
		{name: "seed-one", metrics: &storage.NodeMetrics{Height: 100}},
	}

	result := f.Filter("pocket", nodes)
	if len(result) != 1 {
		t.Fatalf("nil store should pass through, got %d nodes", len(result))
	}
}

func TestArchivalFilter_EmptyNodes(t *testing.T) {
	t.Parallel()

	archStore := storage.NewArchivalStore()
	f := NewArchivalFilter(archStore, zap.NewNop())

	result := f.Filter("pocket", nil)
	if len(result) != 0 {
		t.Fatalf("expected 0 nodes, got %d", len(result))
	}
}

func TestArchivalFilter_IndependentNetworks(t *testing.T) {
	t.Parallel()

	archStore := storage.NewArchivalStore()
	archStore.SetArchival("pocket", "node-1")
	archStore.SetNotArchival("ethereum", "node-1")

	f := NewArchivalFilter(archStore, zap.NewNop())

	pocketNodes := []nodeWithName{
		{name: "node-1", metrics: &storage.NodeMetrics{Height: 100}},
	}
	ethNodes := []nodeWithName{
		{name: "node-1", metrics: &storage.NodeMetrics{Height: 20000000}},
	}

	pResult := f.Filter("pocket", pocketNodes)
	eResult := f.Filter("ethereum", ethNodes)

	if len(pResult) != 1 {
		t.Fatalf("pocket: expected 1 node, got %d", len(pResult))
	}
	if len(eResult) != 0 {
		t.Fatalf("ethereum: expected 0 nodes, got %d", len(eResult))
	}
}

// --- Sync Filter Tests ---

func TestSyncFilter_ExcludesDriftedNodes(t *testing.T) {
	t.Parallel()

	oracleStore := storage.NewOracleStore()
	oracleStore.Update("pocket", 100, "oracle")

	f := NewSyncFilter(oracleStore, zap.NewNop())
	nodes := []nodeWithName{
		{name: "seed-one", metrics: &storage.NodeMetrics{Height: 100}},  // drift=0
		{name: "seed-two", metrics: &storage.NodeMetrics{Height: 94}},   // drift=6 > 5
		{name: "seed-three", metrics: &storage.NodeMetrics{Height: 96}}, // drift=4 <= 5
	}

	result := f.Filter("pocket", 5, nodes)
	if len(result) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(result))
	}

	names := make(map[string]bool)
	for _, n := range result {
		names[n.name] = true
	}
	if !names["seed-one"] {
		t.Error("expected seed-one (drift=0)")
	}
	if !names["seed-three"] {
		t.Error("expected seed-three (drift=4)")
	}
	if names["seed-two"] {
		t.Error("seed-two should be excluded (drift=6)")
	}
}

func TestSyncFilter_ExcludesAheadNodes(t *testing.T) {
	t.Parallel()

	oracleStore := storage.NewOracleStore()
	oracleStore.Update("pocket", 100, "oracle")

	f := NewSyncFilter(oracleStore, zap.NewNop())
	nodes := []nodeWithName{
		{name: "seed-one", metrics: &storage.NodeMetrics{Height: 100}}, // drift=0
		{name: "seed-two", metrics: &storage.NodeMetrics{Height: 110}}, // drift=10 > 5, ahead
	}

	result := f.Filter("pocket", 5, nodes)
	if len(result) != 1 {
		t.Fatalf("expected 1 node, got %d", len(result))
	}
	if result[0].name != "seed-one" {
		t.Fatalf("expected seed-one, got %s", result[0].name)
	}
}

func TestSyncFilter_ExactlyAtDrift(t *testing.T) {
	t.Parallel()

	oracleStore := storage.NewOracleStore()
	oracleStore.Update("pocket", 100, "oracle")

	f := NewSyncFilter(oracleStore, zap.NewNop())
	nodes := []nodeWithName{
		{name: "seed-one", metrics: &storage.NodeMetrics{Height: 95}}, // drift=5, exactly at limit
	}

	result := f.Filter("pocket", 5, nodes)
	if len(result) != 1 {
		t.Fatalf("expected 1 node (drift exactly at max), got %d", len(result))
	}
}

func TestSyncFilter_AllDrifted(t *testing.T) {
	t.Parallel()

	oracleStore := storage.NewOracleStore()
	oracleStore.Update("pocket", 100, "oracle")

	f := NewSyncFilter(oracleStore, zap.NewNop())
	nodes := []nodeWithName{
		{name: "seed-one", metrics: &storage.NodeMetrics{Height: 50}},  // drift=50
		{name: "seed-two", metrics: &storage.NodeMetrics{Height: 200}}, // drift=100
	}

	result := f.Filter("pocket", 5, nodes)
	if len(result) != 0 {
		t.Fatalf("expected 0 nodes, got %d", len(result))
	}
}

func TestSyncFilter_NoOracleData(t *testing.T) {
	t.Parallel()

	oracleStore := storage.NewOracleStore()
	// No oracle data for "pocket".

	f := NewSyncFilter(oracleStore, zap.NewNop())
	nodes := []nodeWithName{
		{name: "seed-one", metrics: &storage.NodeMetrics{Height: 100}},
	}

	result := f.Filter("pocket", 5, nodes)
	if len(result) != 1 {
		t.Fatalf("no oracle data should pass through, got %d nodes", len(result))
	}
}

func TestSyncFilter_ZeroMaxDrift(t *testing.T) {
	t.Parallel()

	oracleStore := storage.NewOracleStore()
	oracleStore.Update("pocket", 100, "oracle")

	f := NewSyncFilter(oracleStore, zap.NewNop())
	nodes := []nodeWithName{
		{name: "seed-one", metrics: &storage.NodeMetrics{Height: 99}},
	}

	result := f.Filter("pocket", 0, nodes)
	if len(result) != 1 {
		t.Fatalf("zero maxDrift should disable filter, got %d nodes", len(result))
	}
}

func TestSyncFilter_NegativeMaxDrift(t *testing.T) {
	t.Parallel()

	oracleStore := storage.NewOracleStore()
	oracleStore.Update("pocket", 100, "oracle")

	f := NewSyncFilter(oracleStore, zap.NewNop())
	nodes := []nodeWithName{
		{name: "seed-one", metrics: &storage.NodeMetrics{Height: 99}},
	}

	result := f.Filter("pocket", -1, nodes)
	if len(result) != 1 {
		t.Fatalf("negative maxDrift should disable filter, got %d nodes", len(result))
	}
}

func TestSyncFilter_NilFilter(t *testing.T) {
	t.Parallel()

	var f *SyncFilter
	nodes := []nodeWithName{
		{name: "seed-one", metrics: &storage.NodeMetrics{Height: 100}},
	}

	result := f.Filter("pocket", 5, nodes)
	if len(result) != 1 {
		t.Fatalf("nil filter should pass through, got %d nodes", len(result))
	}
}

func TestSyncFilter_NilStore(t *testing.T) {
	t.Parallel()

	f := NewSyncFilter(nil, zap.NewNop())
	nodes := []nodeWithName{
		{name: "seed-one", metrics: &storage.NodeMetrics{Height: 100}},
	}

	result := f.Filter("pocket", 5, nodes)
	if len(result) != 1 {
		t.Fatalf("nil store should pass through, got %d nodes", len(result))
	}
}

func TestSyncFilter_EmptyNodes(t *testing.T) {
	t.Parallel()

	oracleStore := storage.NewOracleStore()
	oracleStore.Update("pocket", 100, "oracle")

	f := NewSyncFilter(oracleStore, zap.NewNop())
	result := f.Filter("pocket", 5, nil)
	if len(result) != 0 {
		t.Fatalf("expected 0 nodes, got %d", len(result))
	}
}

func TestSyncFilter_IndependentNetworks(t *testing.T) {
	t.Parallel()

	oracleStore := storage.NewOracleStore()
	oracleStore.Update("pocket", 100, "oracle-pocket")
	oracleStore.Update("ethereum", 20000000, "oracle-eth")

	f := NewSyncFilter(oracleStore, zap.NewNop())

	pocketNodes := []nodeWithName{
		{name: "node-1", metrics: &storage.NodeMetrics{Height: 90}}, // drift=10 > 5
	}
	ethNodes := []nodeWithName{
		{name: "node-1", metrics: &storage.NodeMetrics{Height: 19999998}}, // drift=2 <= 5
	}

	pResult := f.Filter("pocket", 5, pocketNodes)
	eResult := f.Filter("ethereum", 5, ethNodes)

	if len(pResult) != 0 {
		t.Fatalf("pocket: expected 0 (drifted), got %d", len(pResult))
	}
	if len(eResult) != 1 {
		t.Fatalf("ethereum: expected 1 (within drift), got %d", len(eResult))
	}
}

// --- Integration: Selector with both filters ---

func TestSelector_WithArchivalAndSyncFilters(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	archivalStore := storage.NewArchivalStore()
	oracleStore := storage.NewOracleStore()
	configLoader := createTestConfig(t, 2)

	// 3 nodes all at height 100.
	heightStore.Update("pocket", "node-1", "api", 100, 50*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "api", 100, 30*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-3", "api", 90, 40*time.Millisecond, "internal") // drifted

	// node-1 is archival, node-2 is not, node-3 is archival.
	archivalStore.SetArchival("pocket", "node-1")
	archivalStore.SetNotArchival("pocket", "node-2")
	archivalStore.SetArchival("pocket", "node-3")

	// Oracle at 100, max drift 5. node-3 (height 90) drift=10 > 5.
	oracleStore.Update("pocket", 100, "oracle")

	sel := NewSelector(heightStore, nil, configLoader, logger)
	sel.SetArchivalFilter(NewArchivalFilter(archivalStore, logger))
	sel.SetSyncFilter(NewSyncFilter(oracleStore, logger))
	sel.SetSyncMaxDrift("pocket", 5)

	m, nodeName, decision := sel.GetBestNode("pocket", "api")

	if m == nil {
		t.Fatal("expected a node to be selected")
	}
	// node-1 is the only one that passes all filters:
	// - archival: yes
	// - sync: drift=0 <= 5
	// node-2 fails archival, node-3 fails sync (drift=10)
	if nodeName != "node-1" {
		t.Fatalf("expected node-1, got %s", nodeName)
	}
	if decision.Candidates != 1 {
		t.Fatalf("expected 1 candidate, got %d", decision.Candidates)
	}
}

func TestSelector_FiltersDisabled_NoFiltering(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	configLoader := createTestConfig(t, 2)

	heightStore.Update("pocket", "node-1", "api", 100, 50*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "api", 100, 30*time.Millisecond, "internal")

	// No filters set — V1 behavior.
	sel := NewSelector(heightStore, nil, configLoader, logger)

	_, _, decision := sel.GetBestNode("pocket", "api")
	if decision.Candidates != 2 {
		t.Fatalf("expected 2 candidates without filters, got %d", decision.Candidates)
	}
}
