package selector

import (
	"sync"
	"testing"
	"time"

	"sauron/config"
	"sauron/internal/testutil"
	"sauron/storage"

	"go.uber.org/zap"
)

// v2TestYAML is the canonical V2 multi-chain config used across all V2 pipeline tests.
// It has two networks: "pocket" (cosmos, height_check via rpc) and "ethereum" (evm, height_check via jsonrpc).
const v2TestYAML = `listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rest
        listen: ":8080"
      - protocol: rpc
        listen: ":8081"
      - protocol: grpc
        listen: ":8082"
  - name: ethereum
    type: evm
    height_check:
      interval: 12s
    endpoints:
      - protocol: jsonrpc
        listen: ":9080"
internals:
  - name: node-1
    network: pocket
    endpoints:
      rest: "http://node1.example.com:1317"
      rpc: "http://node1.example.com:26657"
      grpc: "node1.example.com:9090"
  - name: node-2
    network: pocket
    endpoints:
      rest: "http://node2.example.com:1317"
      rpc: "http://node2.example.com:26657"
      grpc: "node2.example.com:9090"
  - name: eth-node-1
    network: ethereum
    endpoints:
      jsonrpc: "http://geth1.example.com:8545"
`

// createV2TestConfig writes v2TestYAML to a temp file and returns a loaded MultiChainLoader.
func createV2TestConfig(t *testing.T) *config.MultiChainLoader {
	t.Helper()
	return testutil.NewMultiChainLoader(t, v2TestYAML)
}

// setupV2Pipeline creates the minimal V2 selector wiring: HeightStore + HealthStore + MultiChainLoader.
// No endpointStore (externals), no archival/sync filters — pure V2 height/health separation.
func setupV2Pipeline(t *testing.T) (*Selector, *storage.HeightStore, *storage.HealthStore) {
	t.Helper()

	heightStore := storage.NewHeightStore()
	healthStore := storage.NewHealthStore()
	loader := createV2TestConfig(t)
	logger := zap.NewNop()

	sel := NewSelector(heightStore, nil, loader, logger)
	sel.SetHealthStore(healthStore)
	return sel, heightStore, healthStore
}

// TestV2Pipeline_HeightUnderRPC_RequestForRest_Healthy proves the core V2 model:
// height stored only under "rpc" (height-check protocol), and a "rest" request
// finds the node by looking up rpc heights then filtering by rest health.
func TestV2Pipeline_HeightUnderRPC_RequestForRest_Healthy(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore := setupV2Pipeline(t)

	// Height stored under the height-check protocol ("rpc"), NOT under "rest".
	heightStore.Update("pocket", "node-1", "rpc", 100, 10*time.Millisecond, "internal")

	// REST endpoint is healthy.
	healthStore.SetHealthy("pocket", "node-1", "rest")

	m, nodeName, decision := sel.GetBestNode("pocket", "rest")

	if m == nil {
		t.Fatal("expected node-1 to be selected: height found via rpc, rest is healthy")
	}
	if nodeName != "node-1" {
		t.Fatalf("expected node-1, got %q", nodeName)
	}
	if m.Height != 100 {
		t.Fatalf("expected height=100, got %d", m.Height)
	}
	if decision == nil {
		t.Fatal("expected non-nil SelectionDecision")
	}
	if decision.MaxHeight != 100 {
		t.Fatalf("expected decision.MaxHeight=100, got %d", decision.MaxHeight)
	}
	if decision.Candidates != 1 {
		t.Fatalf("expected 1 candidate, got %d", decision.Candidates)
	}
}

// TestV2Pipeline_HeightUnderRPC_RequestForRest_Unhealthy proves that when rest
// is unhealthy, the selector returns nil even though height data exists via rpc.
func TestV2Pipeline_HeightUnderRPC_RequestForRest_Unhealthy(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore := setupV2Pipeline(t)

	heightStore.Update("pocket", "node-1", "rpc", 100, 10*time.Millisecond, "internal")
	healthStore.SetUnhealthy("pocket", "node-1", "rest", "connection refused")

	m, nodeName, _ := sel.GetBestNode("pocket", "rest")

	if m != nil {
		t.Fatalf("expected nil: rest is unhealthy; got node=%q height=%d", nodeName, m.Height)
	}
}

// TestV2Pipeline_HeightUnderRPC_RequestForGRPC_Healthy proves height-check via rpc
// also correctly gates grpc requests through grpc health status.
func TestV2Pipeline_HeightUnderRPC_RequestForGRPC_Healthy(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore := setupV2Pipeline(t)

	heightStore.Update("pocket", "node-1", "rpc", 100, 10*time.Millisecond, "internal")
	healthStore.SetHealthy("pocket", "node-1", "grpc")

	m, nodeName, decision := sel.GetBestNode("pocket", "grpc")

	if m == nil {
		t.Fatal("expected node-1: height via rpc, grpc is healthy")
	}
	if nodeName != "node-1" {
		t.Fatalf("expected node-1, got %q", nodeName)
	}
	if m.Height != 100 {
		t.Fatalf("expected height=100, got %d", m.Height)
	}
	if decision.MaxHeight != 100 {
		t.Fatalf("expected MaxHeight=100, got %d", decision.MaxHeight)
	}
}

// TestV2Pipeline_HeightUnderRPC_RequestForGRPC_NeverChecked proves that a node
// whose grpc endpoint has never been health-checked is treated as unhealthy
// (HealthStore.IsHealthy returns false for never-checked nodes).
func TestV2Pipeline_HeightUnderRPC_RequestForGRPC_NeverChecked(t *testing.T) {
	t.Parallel()

	sel, heightStore, _ := setupV2Pipeline(t)

	// Height exists but grpc was never checked — HealthStore has no entry.
	heightStore.Update("pocket", "node-1", "rpc", 100, 10*time.Millisecond, "internal")

	m, nodeName, _ := sel.GetBestNode("pocket", "grpc")

	if m != nil {
		t.Fatalf("expected nil: grpc never health-checked; got node=%q", nodeName)
	}
}

// TestV2Pipeline_MultipleNodes_MixedHealth proves that only healthy nodes are
// selected when multiple nodes share the same height.
func TestV2Pipeline_MultipleNodes_MixedHealth(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore := setupV2Pipeline(t)

	// Both at same height, stored under height-check protocol.
	heightStore.Update("pocket", "node-1", "rpc", 100, 10*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "rpc", 100, 10*time.Millisecond, "internal")

	// Only node-1's rest is healthy.
	healthStore.SetHealthy("pocket", "node-1", "rest")
	healthStore.SetUnhealthy("pocket", "node-2", "rest", "timeout")

	m, nodeName, decision := sel.GetBestNode("pocket", "rest")

	if m == nil {
		t.Fatal("expected node-1 to be selected")
	}
	if nodeName != "node-1" {
		t.Fatalf("expected node-1 (only healthy node), got %q", nodeName)
	}
	if m.Height != 100 {
		t.Fatalf("expected height=100, got %d", m.Height)
	}
	// Only 1 candidate survived after health filter.
	if decision.Candidates != 1 {
		t.Fatalf("expected 1 candidate after health filter, got %d", decision.Candidates)
	}
}

// TestV2Pipeline_GetEndpointURL verifies that GetEndpointURL returns
// the correct URL for each V2 protocol name.
func TestV2Pipeline_GetEndpointURL(t *testing.T) {
	t.Parallel()

	sel, _, _ := setupV2Pipeline(t)

	tests := []struct {
		node     string
		protocol string
		wantURL  string
	}{
		{"node-1", "rest", "http://node1.example.com:1317"},
		{"node-1", "rpc", "http://node1.example.com:26657"},
		// gRPC returned as-is (no trailing slash normalization).
		{"node-1", "grpc", "node1.example.com:9090"},
		{"node-2", "rest", "http://node2.example.com:1317"},
		{"node-2", "grpc", "node2.example.com:9090"},
	}

	for _, tt := range tests {
		got := sel.GetEndpointURL(tt.node, tt.protocol)
		if got != tt.wantURL {
			t.Errorf("GetEndpointURL(%q, %q): got %q, want %q", tt.node, tt.protocol, got, tt.wantURL)
		}
	}
}

// TestV2Pipeline_GetHighestHeights_AllTypesShareRPCHeight proves that
// GetHighestHeights returns the same height for all protocol types because
// internally it always queries the height-check protocol ("rpc" for cosmos).
func TestV2Pipeline_GetHighestHeights_AllTypesShareRPCHeight(t *testing.T) {
	t.Parallel()

	sel, heightStore, _ := setupV2Pipeline(t)

	// Height stored only under "rpc" (height-check protocol).
	heightStore.Update("pocket", "node-1", "rpc", 100, 10*time.Millisecond, "internal")

	enabledTypes := []string{"rest", "rpc", "grpc"}
	heights := sel.GetHighestHeights("pocket", enabledTypes)

	if len(heights) != 3 {
		t.Fatalf("expected heights for all 3 types, got %d: %v", len(heights), heights)
	}
	for _, typ := range enabledTypes {
		h, ok := heights[typ]
		if !ok {
			t.Errorf("expected height for type %q, not present in result", typ)
			continue
		}
		if h != 100 {
			t.Errorf("expected height=100 for type %q, got %d", typ, h)
		}
	}
}

// TestV2Pipeline_EVMNetwork_JSONRPCHeightCheck proves that EVM networks use
// "jsonrpc" as the height-check protocol and route correctly through that.
func TestV2Pipeline_EVMNetwork_JSONRPCHeightCheck(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore := setupV2Pipeline(t)

	// EVM height stored under "jsonrpc" (height-check protocol for evm type).
	heightStore.Update("ethereum", "eth-node-1", "jsonrpc", 5000, 5*time.Millisecond, "internal")
	healthStore.SetHealthy("ethereum", "eth-node-1", "jsonrpc")

	m, nodeName, decision := sel.GetBestNode("ethereum", "jsonrpc")

	if m == nil {
		t.Fatal("expected eth-node-1 to be selected")
	}
	if nodeName != "eth-node-1" {
		t.Fatalf("expected eth-node-1, got %q", nodeName)
	}
	if m.Height != 5000 {
		t.Fatalf("expected height=5000, got %d", m.Height)
	}
	if decision.MaxHeight != 5000 {
		t.Fatalf("expected MaxHeight=5000, got %d", decision.MaxHeight)
	}
}

// TestV2Pipeline_NoHeightData_ReturnsNil proves that with no height data at all,
// GetBestNode returns nil regardless of health status.
func TestV2Pipeline_NoHeightData_ReturnsNil(t *testing.T) {
	t.Parallel()

	sel, _, healthStore := setupV2Pipeline(t)

	// Health set but no height data.
	healthStore.SetHealthy("pocket", "node-1", "rest")

	m, _, _ := sel.GetBestNode("pocket", "rest")
	if m != nil {
		t.Fatal("expected nil: no height data present")
	}
}

// TestV2Pipeline_HeightOnlyUnderWrongProtocol_NotFound proves that if height
// is stored under "rest" but the height-check protocol is "rpc", the selector
// will not find any nodes (since it only looks under the height-check protocol).
func TestV2Pipeline_HeightOnlyUnderWrongProtocol_NotFound(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore := setupV2Pipeline(t)

	// Height stored under "rest" instead of "rpc" — simulates a misconfigured checker.
	heightStore.Update("pocket", "node-1", "rest", 100, 10*time.Millisecond, "internal")
	healthStore.SetHealthy("pocket", "node-1", "rest")

	m, _, _ := sel.GetBestNode("pocket", "rest")
	if m != nil {
		t.Fatal("expected nil: height stored under 'rest' but height-check protocol is 'rpc'")
	}
}

// TestV2Pipeline_RoundRobinAmongHealthyNodes proves round-robin distribution
// works correctly after the health filter reduces the candidate set.
func TestV2Pipeline_RoundRobinAmongHealthyNodes(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore := setupV2Pipeline(t)

	// Both nodes at same height.
	heightStore.Update("pocket", "node-1", "rpc", 100, 10*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "rpc", 100, 10*time.Millisecond, "internal")

	// Both rest endpoints healthy.
	healthStore.SetHealthy("pocket", "node-1", "rest")
	healthStore.SetHealthy("pocket", "node-2", "rest")

	seen := make(map[string]int)
	const iterations = 10
	for i := 0; i < iterations; i++ {
		_, nodeName, _ := sel.GetBestNode("pocket", "rest")
		if nodeName == "" {
			t.Fatalf("iteration %d: expected a node, got empty string", i)
		}
		seen[nodeName]++
	}

	if len(seen) != 2 {
		t.Fatalf("expected both nodes to be selected in round-robin, got: %v", seen)
	}
	if seen["node-1"] == 0 {
		t.Error("node-1 was never selected")
	}
	if seen["node-2"] == 0 {
		t.Error("node-2 was never selected")
	}
}

// TestV2Pipeline_GetHighestHeights_NoData_ReturnsEmpty proves GetHighestHeights
// returns an empty map when no height data exists for the network.
func TestV2Pipeline_GetHighestHeights_NoData_ReturnsEmpty(t *testing.T) {
	t.Parallel()

	sel, _, _ := setupV2Pipeline(t)

	heights := sel.GetHighestHeights("pocket", []string{"rest", "rpc", "grpc"})
	if len(heights) != 0 {
		t.Fatalf("expected empty map with no height data, got: %v", heights)
	}
}

// TestV2Pipeline_TwoNetworksIndependent proves that pocket and ethereum network
// configurations do not interfere with each other.
func TestV2Pipeline_TwoNetworksIndependent(t *testing.T) {
	t.Parallel()

	sel, heightStore, healthStore := setupV2Pipeline(t)

	// pocket: node-1 healthy rest.
	heightStore.Update("pocket", "node-1", "rpc", 100, 10*time.Millisecond, "internal")
	healthStore.SetHealthy("pocket", "node-1", "rest")

	// ethereum: eth-node-1 healthy jsonrpc.
	heightStore.Update("ethereum", "eth-node-1", "jsonrpc", 5000, 5*time.Millisecond, "internal")
	healthStore.SetHealthy("ethereum", "eth-node-1", "jsonrpc")

	// pocket rest request.
	m, nodeName, _ := sel.GetBestNode("pocket", "rest")
	if m == nil {
		t.Fatal("pocket: expected node-1")
	}
	if nodeName != "node-1" {
		t.Fatalf("pocket: expected node-1, got %q", nodeName)
	}
	if m.Height != 100 {
		t.Fatalf("pocket: expected height=100, got %d", m.Height)
	}

	// ethereum jsonrpc request.
	m2, nodeName2, _ := sel.GetBestNode("ethereum", "jsonrpc")
	if m2 == nil {
		t.Fatal("ethereum: expected eth-node-1")
	}
	if nodeName2 != "eth-node-1" {
		t.Fatalf("ethereum: expected eth-node-1, got %q", nodeName2)
	}
	if m2.Height != 5000 {
		t.Fatalf("ethereum: expected height=5000, got %d", m2.Height)
	}
}

// TestV2Pipeline_GetEndpointURL_UnknownNode_ReturnsEmpty proves that querying
// a non-existent node returns an empty string without panic.
func TestV2Pipeline_GetEndpointURL_UnknownNode_ReturnsEmpty(t *testing.T) {
	t.Parallel()

	sel, _, _ := setupV2Pipeline(t)

	got := sel.GetEndpointURL("does-not-exist", "rest")
	if got != "" {
		t.Fatalf("expected empty string for unknown node, got %q", got)
	}
}

// TestV2Pipeline_ConfigLoadsCorrectly proves V2 config is loaded and accessible
// with expected network and node values.
func TestV2Pipeline_ConfigLoadsCorrectly(t *testing.T) {
	t.Parallel()

	loader := createV2TestConfig(t)
	cfg := loader.Get()

	if cfg == nil {
		t.Fatal("expected non-nil config")
	}

	// Verify networks.
	if len(cfg.Networks) != 2 {
		t.Fatalf("expected 2 networks, got %d", len(cfg.Networks))
	}

	pocket := cfg.FindNetwork("pocket")
	if pocket == nil {
		t.Fatal("expected 'pocket' network in config")
	}
	if pocket.Type != "cosmos" {
		t.Fatalf("pocket: expected type=cosmos, got %q", pocket.Type)
	}
	if pocket.HeightCheck == nil {
		t.Fatal("pocket: expected HeightCheck to be set")
	}
	if pocket.HeightCheckProtocol() != "rpc" {
		t.Fatalf("pocket: expected height-check protocol=rpc, got %q", pocket.HeightCheckProtocol())
	}

	eth := cfg.FindNetwork("ethereum")
	if eth == nil {
		t.Fatal("expected 'ethereum' network in config")
	}
	if eth.Type != "evm" {
		t.Fatalf("ethereum: expected type=evm, got %q", eth.Type)
	}

	// Verify internals.
	if len(cfg.Internals) != 3 {
		t.Fatalf("expected 3 internals, got %d", len(cfg.Internals))
	}

	// Find node-1 and verify all endpoints.
	var node1 *config.MultiChainNode
	for i := range cfg.Internals {
		if cfg.Internals[i].Name == "node-1" {
			node1 = &cfg.Internals[i]
			break
		}
	}
	if node1 == nil {
		t.Fatal("expected node-1 in internals")
	}
	if node1.Endpoints["rest"] != "http://node1.example.com:1317" {
		t.Errorf("node-1 rest: got %q, want %q", node1.Endpoints["rest"], "http://node1.example.com:1317")
	}
	if node1.Endpoints["rpc"] != "http://node1.example.com:26657" {
		t.Errorf("node-1 rpc: got %q, want %q", node1.Endpoints["rpc"], "http://node1.example.com:26657")
	}
	if node1.Endpoints["grpc"] != "node1.example.com:9090" {
		t.Errorf("node-1 grpc: got %q, want %q", node1.Endpoints["grpc"], "node1.example.com:9090")
	}
}

// TestV2Pipeline_ConcurrentGetBestNode verifies that concurrent calls to
// GetBestNode (which internally calls heightCheckProtocol and reads from
// HeightStore + HealthStore) do not race.
// This simulates the production scenario where multiple proxy goroutines
// call GetBestNode simultaneously.
func TestV2Pipeline_ConcurrentGetBestNode(t *testing.T) {
	sel, heightStore, healthStore := setupV2Pipeline(t)

	// Seed data for both networks.
	heightStore.Update("pocket", "node-1", "rpc", 100, 5*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "rpc", 100, 5*time.Millisecond, "internal")
	heightStore.Update("ethereum", "eth-node-1", "jsonrpc", 5000, 3*time.Millisecond, "internal")

	healthStore.SetHealthy("pocket", "node-1", "rest")
	healthStore.SetHealthy("pocket", "node-1", "rpc")
	healthStore.SetHealthy("pocket", "node-1", "grpc")
	healthStore.SetHealthy("pocket", "node-2", "rest")
	healthStore.SetHealthy("pocket", "node-2", "rpc")
	healthStore.SetHealthy("pocket", "node-2", "grpc")
	healthStore.SetHealthy("ethereum", "eth-node-1", "jsonrpc")

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	// Mix of different networks and protocols to stress all code paths.
	requests := []struct {
		network      string
		endpointType string
	}{
		{"pocket", "rest"},
		{"pocket", "rpc"},
		{"pocket", "grpc"},

		{"ethereum", "jsonrpc"},
	}

	for i := 0; i < goroutines; i++ {
		req := requests[i%len(requests)]
		go func() {
			defer wg.Done()
			// Each goroutine calls GetBestNode multiple times.
			for j := 0; j < 50; j++ {
				node, name, decision := sel.GetBestNode(req.network, req.endpointType)
				if node == nil {
					t.Errorf("goroutine got nil node for %s/%s", req.network, req.endpointType)
					return
				}
				if name == "" {
					t.Errorf("goroutine got empty name for %s/%s", req.network, req.endpointType)
					return
				}
				if decision == nil {
					t.Errorf("goroutine got nil decision for %s/%s", req.network, req.endpointType)
					return
				}
			}
		}()
	}

	wg.Wait()
}
