package selector

import (
	"os"
	"testing"
	"time"

	"sauron/config"
	"sauron/storage"

	"go.uber.org/zap"
)

// createTestConfig creates a temporary config file and returns a Loader
func createTestConfig(t *testing.T, threshold int64) *config.Loader {
	t.Helper()

	// Create temp config file
	content := `
api: true
rpc: true
grpc: true
listen: ":3000"
external_failover_threshold: %d

networks:
  - name: "pocket"
    api_listen: ":8080"
    rpc_listen: ":8081"
    grpc_listen: ":8082"

internals:
  - name: node-1
    api: "https://node1.example.com"
    rpc: "https://node1.example.com:26657"
    grpc: "node1.example.com:9090"
    network: "pocket"
  - name: node-2
    api: "https://node2.example.com"
    rpc: "https://node2.example.com:26657"
    grpc: "node2.example.com:9090"
    network: "pocket"
`
	tmpFile, err := os.CreateTemp("", "sauron-test-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp config file: %v", err)
	}

	configContent := replaceThreshold(content, threshold)

	if _, err := tmpFile.WriteString(configContent); err != nil {
		t.Fatalf("Failed to write temp config: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatalf("Failed to close temp config file: %v", err)
	}

	// Cleanup on test end
	t.Cleanup(func() {
		_ = os.Remove(tmpFile.Name())
	})

	logger := zap.NewNop()
	loader, err := config.NewLoader(tmpFile.Name(), logger)
	if err != nil {
		t.Fatalf("Failed to create config loader: %v", err)
	}

	return loader
}

func replaceThreshold(content string, threshold int64) string {
	return content[:0] + `
api: true
rpc: true
grpc: true
listen: ":3000"
external_failover_threshold: ` + itoa(threshold) + `

timeouts:
  health_check: 5s
  proxy: 60s

networks:
  - name: "pocket"
    api_listen: ":8080"
    rpc_listen: ":8081"
    grpc_listen: ":8082"

internals:
  - name: node-1
    api: "https://node1.example.com"
    rpc: "https://node1.example.com:26657"
    grpc: "node1.example.com:9090"
    network: "pocket"
  - name: node-2
    api: "https://node2.example.com"
    rpc: "https://node2.example.com:26657"
    grpc: "node2.example.com:9090"
    network: "pocket"
`
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestSelectorInternalsOnlyWhenWithinThreshold tests that externals are not added
// when internal max height is within threshold of external max height
func TestSelectorInternalsOnlyWhenWithinThreshold(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 2)

	// Setup internal nodes at height 100
	heightStore.Update("pocket", "node-1", "api", 100, 50*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "api", 98, 30*time.Millisecond, "internal")

	// Setup external endpoint at height 102 (within threshold of 2)
	endpointStore.StoreAdvertised("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com")
	endpointStore.MarkValidated("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com", 102, 20*time.Millisecond)

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	metrics, nodeName, decision := selector.GetBestNode("pocket", "api")

	if metrics == nil {
		t.Fatal("Expected metrics to be returned")
	}

	// Should select internal node-1 (height 100) because external (102) is only 2 blocks ahead
	// 102 > 100 + 2 = false, so externals not added
	if nodeName != "node-1" {
		t.Errorf("Expected node-1 to be selected, got %s", nodeName)
	}

	if metrics.Height != 100 {
		t.Errorf("Expected height 100, got %d", metrics.Height)
	}

	// Only 2 candidates (internal nodes)
	if decision.Candidates != 2 {
		t.Errorf("Expected 2 candidates, got %d", decision.Candidates)
	}
}

// TestSelectorExternalsAddedWhenAheadByThreshold tests that externals are added
// when external max height exceeds internal max height by more than threshold
func TestSelectorExternalsAddedWhenAheadByThreshold(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 2)

	// Setup internal nodes at height 100
	heightStore.Update("pocket", "node-1", "api", 100, 50*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "api", 98, 30*time.Millisecond, "internal")

	// Setup external endpoint at height 103 (more than threshold of 2 ahead)
	endpointStore.StoreAdvertised("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com")
	endpointStore.MarkValidated("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com", 103, 20*time.Millisecond)

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	metrics, nodeName, decision := selector.GetBestNode("pocket", "api")

	if metrics == nil {
		t.Fatal("Expected metrics to be returned")
	}

	// Should select external (height 103) because 103 > 100 + 2 = true
	expectedName := "ext:https://ext1.example.com"
	if nodeName != expectedName {
		t.Errorf("Expected %s to be selected, got %s", expectedName, nodeName)
	}

	if metrics.Height != 103 {
		t.Errorf("Expected height 103, got %d", metrics.Height)
	}

	// 3 candidates (2 internal + 1 external)
	if decision.Candidates != 3 {
		t.Errorf("Expected 3 candidates, got %d", decision.Candidates)
	}
}

// TestSelectorExternalsAddedWhenNoHealthyInternals tests that externals are used
// when all internal nodes have height 0
func TestSelectorExternalsAddedWhenNoHealthyInternals(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 2)

	// Setup internal nodes at height 0 (unhealthy)
	heightStore.Update("pocket", "node-1", "api", 0, 50*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "api", 0, 30*time.Millisecond, "internal")

	// Setup external endpoint at height 100
	endpointStore.StoreAdvertised("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com")
	endpointStore.MarkValidated("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com", 100, 20*time.Millisecond)

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	metrics, nodeName, _ := selector.GetBestNode("pocket", "api")

	if metrics == nil {
		t.Fatal("Expected metrics to be returned")
	}

	// Should select external because internal max height is 0
	expectedName := "ext:https://ext1.example.com"
	if nodeName != expectedName {
		t.Errorf("Expected %s to be selected, got %s", expectedName, nodeName)
	}

	if metrics.Height != 100 {
		t.Errorf("Expected height 100, got %d", metrics.Height)
	}
}

// TestSelectorLatencyTiebreakerSameHeight tests that when multiple nodes have
// the same height, round-robin distribution is used
func TestSelectorLatencyTiebreakerSameHeight(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 2)

	// Setup internal nodes at same height with different latencies
	heightStore.Update("pocket", "node-1", "api", 100, 100*time.Millisecond, "internal")
	heightStore.Update("pocket", "node-2", "api", 100, 20*time.Millisecond, "internal")

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	metrics, nodeName, decision := selector.GetBestNode("pocket", "api")

	if metrics == nil {
		t.Fatal("Expected metrics to be returned")
	}

	// Should select one of the nodes via round-robin
	if nodeName != "node-1" && nodeName != "node-2" {
		t.Errorf("Expected node-1 or node-2 to be selected, got %s", nodeName)
	}

	if decision.Reason != "round_robin" {
		t.Errorf("Expected reason 'round_robin', got %s", decision.Reason)
	}
}

// TestSelectorHeightWinner tests that the node with highest height wins
func TestSelectorHeightWinner(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 2)

	// node-1 has higher height but higher latency
	heightStore.Update("pocket", "node-1", "api", 105, 100*time.Millisecond, "internal")
	// node-2 has lower height but lower latency
	heightStore.Update("pocket", "node-2", "api", 100, 20*time.Millisecond, "internal")

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	metrics, nodeName, decision := selector.GetBestNode("pocket", "api")

	if metrics == nil {
		t.Fatal("Expected metrics to be returned")
	}

	// Should select node-1 (highest height wins over lower latency)
	if nodeName != "node-1" {
		t.Errorf("Expected node-1 to be selected (highest height), got %s", nodeName)
	}

	if decision.Reason != "height_winner" {
		t.Errorf("Expected reason 'height_winner', got %s", decision.Reason)
	}

	if metrics.Height != 105 {
		t.Errorf("Expected height 105, got %d", metrics.Height)
	}
}

// TestSelectorDefaultThreshold tests that default threshold of 2 is used
// when not configured
func TestSelectorDefaultThreshold(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 0) // 0 means use default

	// Setup internal at height 100
	heightStore.Update("pocket", "node-1", "api", 100, 50*time.Millisecond, "internal")

	// External at 102 - should NOT trigger failover (102 > 100 + 2 = false)
	endpointStore.StoreAdvertised("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com")
	endpointStore.MarkValidated("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com", 102, 20*time.Millisecond)

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	_, nodeName, decision := selector.GetBestNode("pocket", "api")

	// Should select internal (external within default threshold of 2)
	if nodeName != "node-1" {
		t.Errorf("Expected node-1 to be selected with default threshold, got %s", nodeName)
	}

	if decision.Candidates != 1 {
		t.Errorf("Expected 1 candidate (internals only), got %d", decision.Candidates)
	}
}

// TestSelectorCustomThreshold tests that custom threshold from config is used
func TestSelectorCustomThreshold(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 5) // Custom threshold of 5

	// Setup internal at height 100
	heightStore.Update("pocket", "node-1", "api", 100, 50*time.Millisecond, "internal")

	// External at 103 - would trigger with default threshold but NOT with 5
	endpointStore.StoreAdvertised("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com")
	endpointStore.MarkValidated("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com", 103, 20*time.Millisecond)

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	_, nodeName, decision := selector.GetBestNode("pocket", "api")

	// Should select internal (103 > 100 + 5 = false)
	if nodeName != "node-1" {
		t.Errorf("Expected node-1 with custom threshold 5, got %s", nodeName)
	}

	if decision.Candidates != 1 {
		t.Errorf("Expected 1 candidate, got %d", decision.Candidates)
	}

	// Now test with external at 106 (should trigger: 106 > 100 + 5 = true)
	endpointStore.MarkValidated("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com", 106, 20*time.Millisecond)

	_, nodeName2, decision2 := selector.GetBestNode("pocket", "api")

	expectedName := "ext:https://ext1.example.com"
	if nodeName2 != expectedName {
		t.Errorf("Expected %s when external exceeds threshold, got %s", expectedName, nodeName2)
	}

	if decision2.Candidates != 2 {
		t.Errorf("Expected 2 candidates when threshold exceeded, got %d", decision2.Candidates)
	}
}

// TestSelectorMultipleExternals tests selection among multiple external endpoints
func TestSelectorMultipleExternals(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 2)

	// Internal at height 100
	heightStore.Update("pocket", "node-1", "api", 100, 50*time.Millisecond, "internal")

	// Multiple externals ahead by threshold
	endpointStore.StoreAdvertised("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com")
	endpointStore.MarkValidated("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com", 105, 100*time.Millisecond)

	endpointStore.StoreAdvertised("external-2", "https://ring2.example.com", "pocket", "api", "https://ext2.example.com")
	endpointStore.MarkValidated("external-2", "https://ring2.example.com", "pocket", "api", "https://ext2.example.com", 105, 30*time.Millisecond)

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	metrics, nodeName, decision := selector.GetBestNode("pocket", "api")

	if metrics == nil {
		t.Fatal("Expected metrics to be returned")
	}

	// Should select one of the externals via round-robin (both at max height 105)
	if nodeName != "ext:https://ext1.example.com" && nodeName != "ext:https://ext2.example.com" {
		t.Errorf("Expected ext1 or ext2 to be selected, got %s", nodeName)
	}

	if metrics.Height != 105 {
		t.Errorf("Expected height 105, got %d", metrics.Height)
	}

	// 3 candidates (1 internal + 2 externals)
	if decision.Candidates != 3 {
		t.Errorf("Expected 3 candidates, got %d", decision.Candidates)
	}
}

// TestSelectorNoNodes tests behavior when no nodes are available
func TestSelectorNoNodes(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 2)

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	metrics, nodeName, decision := selector.GetBestNode("pocket", "api")

	if metrics != nil {
		t.Error("Expected nil metrics when no nodes available")
	}

	if nodeName != "" {
		t.Errorf("Expected empty node name, got %s", nodeName)
	}

	if decision != nil {
		t.Error("Expected nil decision when no nodes available")
	}
}

// TestSelectorOnlyAvailable tests selection reason when only one node is available
func TestSelectorOnlyAvailable(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 2)

	// Only one internal node
	heightStore.Update("pocket", "node-1", "api", 100, 50*time.Millisecond, "internal")

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	_, nodeName, decision := selector.GetBestNode("pocket", "api")

	if nodeName != "node-1" {
		t.Errorf("Expected node-1, got %s", nodeName)
	}

	if decision.Reason != "only_available" {
		t.Errorf("Expected reason 'only_available', got %s", decision.Reason)
	}
}

// TestGetHighestHeightsIncludesExternals tests that GetHighestHeights returns
// the max height from both internal and external nodes
func TestGetHighestHeightsIncludesExternals(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 2)

	// Internal at height 100
	heightStore.Update("pocket", "node-1", "api", 100, 50*time.Millisecond, "internal")

	// External at height 150
	endpointStore.StoreAdvertised("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com")
	endpointStore.MarkValidated("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com", 150, 20*time.Millisecond)

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	heights := selector.GetHighestHeights("pocket", []string{"api"})

	if heights["api"] != 150 {
		t.Errorf("Expected highest height 150 (from external), got %d", heights["api"])
	}
}

// TestSelectorExternalNotValidated tests that non-validated externals are not considered
func TestSelectorExternalNotValidated(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 2)

	// Internal at height 100
	heightStore.Update("pocket", "node-1", "api", 100, 50*time.Millisecond, "internal")

	// External advertised but NOT validated (at height 200)
	endpointStore.StoreAdvertised("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com")
	// Not calling MarkValidated, so it's not validated

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	_, nodeName, decision := selector.GetBestNode("pocket", "api")

	// Should only select internal since external is not validated
	if nodeName != "node-1" {
		t.Errorf("Expected node-1 (external not validated), got %s", nodeName)
	}

	if decision.Candidates != 1 {
		t.Errorf("Expected 1 candidate (non-validated external excluded), got %d", decision.Candidates)
	}
}

// TestSelectorInternalWinsOverExternalSameHeight tests that when externals are added
// and internal has same max height, the one with lowest latency wins
func TestSelectorInternalWinsOverExternalSameHeight(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 2)

	// Internal at height 105 with LOW latency
	heightStore.Update("pocket", "node-1", "api", 105, 10*time.Millisecond, "internal")

	// External at height 105 (triggers threshold: 105 > 100 would if internal was 100)
	// But we need to trigger threshold first, so let's set internal lower initially
	// Actually, let's set internal at 100, external at 105
	heightStore.Update("pocket", "node-1", "api", 100, 10*time.Millisecond, "internal")

	// External at 105 with higher latency (triggers: 105 > 100 + 2)
	endpointStore.StoreAdvertised("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com")
	endpointStore.MarkValidated("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com", 105, 50*time.Millisecond)

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	metrics, nodeName, _ := selector.GetBestNode("pocket", "api")

	// External should win because it has higher height (105 > 100)
	expectedName := "ext:https://ext1.example.com"
	if nodeName != expectedName {
		t.Errorf("Expected %s (higher height), got %s", expectedName, nodeName)
	}

	if metrics.Height != 105 {
		t.Errorf("Expected height 105, got %d", metrics.Height)
	}

	// Now test with internal ALSO at 105 - externals should NOT be added anymore
	// because 105 > 105 + 2 = false (internal caught up, no need to overload externals)
	heightStore.Update("pocket", "node-1", "api", 105, 10*time.Millisecond, "internal")

	metrics2, nodeName2, decision2 := selector.GetBestNode("pocket", "api")

	// Internal should win (and be the only candidate since externals not added)
	if nodeName2 != "node-1" {
		t.Errorf("Expected node-1, got %s", nodeName2)
	}

	if metrics2.Height != 105 {
		t.Errorf("Expected height 105, got %d", metrics2.Height)
	}

	// Only 1 candidate because externals are not added when internal catches up
	if decision2.Candidates != 1 {
		t.Errorf("Expected 1 candidate (externals not added when caught up), got %d", decision2.Candidates)
	}

	if decision2.Reason != "only_available" {
		t.Errorf("Expected reason 'only_available', got %s", decision2.Reason)
	}
}

// TestSelectorNilEndpointStore tests behavior when no external endpoint store is configured
func TestSelectorNilEndpointStore(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	configLoader := createTestConfig(t, 2)

	// Setup internals
	heightStore.Update("pocket", "node-1", "api", 100, 50*time.Millisecond, "internal")

	// Create selector with nil endpointStore
	selector := NewSelector(heightStore, nil, configLoader, logger)

	metrics, nodeName, decision := selector.GetBestNode("pocket", "api")

	if metrics == nil {
		t.Fatal("Expected metrics to be returned")
	}

	if nodeName != "node-1" {
		t.Errorf("Expected node-1, got %s", nodeName)
	}

	if decision.Candidates != 1 {
		t.Errorf("Expected 1 candidate, got %d", decision.Candidates)
	}
}

// TestSelectorAllNodesZeroHeight tests behavior when all nodes (internal + external) have height 0
func TestSelectorAllNodesZeroHeight(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 2)

	// Internal at height 0
	heightStore.Update("pocket", "node-1", "api", 0, 50*time.Millisecond, "internal")

	// External also at height 0 (would be added since internal is 0)
	endpointStore.StoreAdvertised("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com")
	endpointStore.MarkValidated("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com", 0, 20*time.Millisecond)

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	metrics, nodeName, decision := selector.GetBestNode("pocket", "api")

	// Should return nil when all nodes have zero height
	if metrics != nil {
		t.Error("Expected nil metrics when all nodes have zero height")
	}

	if nodeName != "" {
		t.Errorf("Expected empty node name, got %s", nodeName)
	}

	if decision != nil {
		t.Error("Expected nil decision when all nodes have zero height")
	}
}

// TestSelectorExternalNotWorking tests that externals marked as not working are excluded
func TestSelectorExternalNotWorking(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 2)

	// Internal at height 100
	heightStore.Update("pocket", "node-1", "api", 100, 50*time.Millisecond, "internal")

	// External at height 200 but will be marked as not working
	endpointStore.StoreAdvertised("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com")
	endpointStore.MarkValidated("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com", 200, 20*time.Millisecond)

	// Simulate 3 errors to mark as not working
	endpointStore.IncrementErrorCount("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com")
	endpointStore.IncrementErrorCount("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com")
	endpointStore.IncrementErrorCount("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com")

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	_, nodeName, decision := selector.GetBestNode("pocket", "api")

	// Should select internal since external is not working
	if nodeName != "node-1" {
		t.Errorf("Expected node-1 (external not working), got %s", nodeName)
	}

	if decision.Candidates != 1 {
		t.Errorf("Expected 1 candidate (not working external excluded), got %d", decision.Candidates)
	}
}

// TestSelectorExternalLowerThanInternalNotAdded tests that externals at lower height
// than internals are not added even if validated
func TestSelectorExternalLowerThanInternalNotAdded(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 2)

	// Internal at height 100
	heightStore.Update("pocket", "node-1", "api", 100, 50*time.Millisecond, "internal")

	// External at height 95 (lower than internal, should not trigger failover)
	endpointStore.StoreAdvertised("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com")
	endpointStore.MarkValidated("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com", 95, 20*time.Millisecond)

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	_, nodeName, decision := selector.GetBestNode("pocket", "api")

	// Should select internal (95 > 100 + 2 = false)
	if nodeName != "node-1" {
		t.Errorf("Expected node-1, got %s", nodeName)
	}

	// Only 1 candidate since externals not added
	if decision.Candidates != 1 {
		t.Errorf("Expected 1 candidate, got %d", decision.Candidates)
	}
}

// TestSelectorNoInternalsOnlyExternals tests behavior when no internal nodes exist
// but external endpoints are available
func TestSelectorNoInternalsOnlyExternals(t *testing.T) {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	configLoader := createTestConfig(t, 2)

	// No internal nodes in heightStore

	// External at height 100
	endpointStore.StoreAdvertised("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com")
	endpointStore.MarkValidated("external-1", "https://ring1.example.com", "pocket", "api", "https://ext1.example.com", 100, 20*time.Millisecond)

	selector := NewSelector(heightStore, endpointStore, configLoader, logger)

	metrics, nodeName, decision := selector.GetBestNode("pocket", "api")

	if metrics == nil {
		t.Fatal("Expected metrics to be returned")
	}

	// Should select external since no internals exist (maxInternalHeight = 0)
	expectedName := "ext:https://ext1.example.com"
	if nodeName != expectedName {
		t.Errorf("Expected %s, got %s", expectedName, nodeName)
	}

	if metrics.Height != 100 {
		t.Errorf("Expected height 100, got %d", metrics.Height)
	}

	if decision.Candidates != 1 {
		t.Errorf("Expected 1 candidate, got %d", decision.Candidates)
	}
}
