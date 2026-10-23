package storage

import (
	"sauron/metrics"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestHealthStore_SetHealthy(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	s.SetHealthy("pocket", "seed-one", "rpc")

	if !s.IsHealthy("pocket", "seed-one", "rpc") {
		t.Fatal("expected healthy after SetHealthy")
	}

	h := s.GetHealth("pocket", "seed-one", "rpc")
	if h == nil {
		t.Fatal("expected non-nil health")
	}
	if !h.Healthy {
		t.Fatal("expected Healthy=true")
	}
	if h.LastError != "" {
		t.Fatalf("expected empty LastError, got %q", h.LastError)
	}
	if h.LastCheck.IsZero() {
		t.Fatal("expected non-zero LastCheck")
	}
}

func TestHealthStore_SetUnhealthy(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	s.SetUnhealthy("pocket", "seed-one", "grpc", "connection refused")

	if s.IsHealthy("pocket", "seed-one", "grpc") {
		t.Fatal("expected unhealthy after SetUnhealthy")
	}

	h := s.GetHealth("pocket", "seed-one", "grpc")
	if h == nil {
		t.Fatal("expected non-nil health")
	}
	if h.Healthy {
		t.Fatal("expected Healthy=false")
	}
	if h.LastError != "connection refused" {
		t.Fatalf("expected 'connection refused', got %q", h.LastError)
	}
}

func TestHealthStore_TransitionHealthyToUnhealthy(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	s.SetHealthy("pocket", "seed-one", "rpc")
	if !s.IsHealthy("pocket", "seed-one", "rpc") {
		t.Fatal("expected healthy")
	}

	s.SetUnhealthy("pocket", "seed-one", "rpc", "timeout")
	if s.IsHealthy("pocket", "seed-one", "rpc") {
		t.Fatal("expected unhealthy after transition")
	}

	h := s.GetHealth("pocket", "seed-one", "rpc")
	if h.LastError != "timeout" {
		t.Fatalf("expected 'timeout', got %q", h.LastError)
	}
}

func TestHealthStore_TransitionUnhealthyToHealthy(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	s.SetUnhealthy("pocket", "seed-one", "rpc", "down")
	s.SetHealthy("pocket", "seed-one", "rpc")

	if !s.IsHealthy("pocket", "seed-one", "rpc") {
		t.Fatal("expected healthy after recovery")
	}

	h := s.GetHealth("pocket", "seed-one", "rpc")
	if h.LastError != "" {
		t.Fatalf("expected empty error after recovery, got %q", h.LastError)
	}
}

func TestHealthStore_NeverChecked(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	if s.IsHealthy("pocket", "seed-one", "rpc") {
		t.Fatal("never-checked endpoint should not be healthy")
	}

	if s.GetHealth("pocket", "seed-one", "rpc") != nil {
		t.Fatal("expected nil for never-checked endpoint")
	}
}

func TestHealthStore_IndependentProtocols(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	s.SetHealthy("pocket", "seed-one", "rpc")
	s.SetHealthy("pocket", "seed-one", "rest")
	s.SetUnhealthy("pocket", "seed-one", "grpc", "broken")

	if !s.IsHealthy("pocket", "seed-one", "rpc") {
		t.Fatal("rpc should be healthy")
	}
	if !s.IsHealthy("pocket", "seed-one", "rest") {
		t.Fatal("rest should be healthy")
	}
	if s.IsHealthy("pocket", "seed-one", "grpc") {
		t.Fatal("grpc should be unhealthy")
	}
}

func TestHealthStore_IndependentNetworks(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	s.SetHealthy("pocket", "node-1", "rpc")
	s.SetUnhealthy("ethereum", "node-1", "rpc", "down")

	if !s.IsHealthy("pocket", "node-1", "rpc") {
		t.Fatal("pocket node-1 rpc should be healthy")
	}
	if s.IsHealthy("ethereum", "node-1", "rpc") {
		t.Fatal("ethereum node-1 rpc should be unhealthy")
	}
}

func TestHealthStore_IndependentNodes(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	s.SetHealthy("pocket", "seed-one", "rpc")
	s.SetUnhealthy("pocket", "seed-two", "rpc", "down")

	if !s.IsHealthy("pocket", "seed-one", "rpc") {
		t.Fatal("seed-one should be healthy")
	}
	if s.IsHealthy("pocket", "seed-two", "rpc") {
		t.Fatal("seed-two should be unhealthy")
	}
}

func TestHealthStore_HealthyNodes(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	s.SetHealthy("pocket", "seed-one", "rpc")
	s.SetHealthy("pocket", "seed-two", "rpc")
	s.SetUnhealthy("pocket", "seed-three", "rpc", "down")
	s.SetHealthy("pocket", "seed-one", "grpc") // different protocol
	s.SetHealthy("ethereum", "eth-1", "rpc")   // different network

	nodes := s.HealthyNodes("pocket", "rpc")
	if len(nodes) != 2 {
		t.Fatalf("expected 2 healthy nodes, got %d: %v", len(nodes), nodes)
	}
	if nodes[0] != "seed-one" || nodes[1] != "seed-two" {
		t.Fatalf("expected [seed-one, seed-two], got %v", nodes)
	}
}

func TestHealthStore_HealthyNodes_Empty(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	nodes := s.HealthyNodes("pocket", "rpc")
	if len(nodes) != 0 {
		t.Fatalf("expected 0 healthy nodes, got %d", len(nodes))
	}
}

func TestHealthStore_HealthyNodes_AllUnhealthy(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	s.SetUnhealthy("pocket", "seed-one", "rpc", "down")
	s.SetUnhealthy("pocket", "seed-two", "rpc", "down")

	nodes := s.HealthyNodes("pocket", "rpc")
	if len(nodes) != 0 {
		t.Fatalf("expected 0 healthy nodes, got %d", len(nodes))
	}
}

func TestHealthStore_HealthyNodes_Sorted(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	// Insert in non-alphabetical order.
	s.SetHealthy("pocket", "zulu", "rpc")
	s.SetHealthy("pocket", "alpha", "rpc")
	s.SetHealthy("pocket", "mike", "rpc")

	nodes := s.HealthyNodes("pocket", "rpc")
	if len(nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(nodes))
	}
	if nodes[0] != "alpha" || nodes[1] != "mike" || nodes[2] != "zulu" {
		t.Fatalf("expected sorted [alpha, mike, zulu], got %v", nodes)
	}
}

func TestHealthStore_AllHealth(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	s.SetHealthy("pocket", "seed-one", "rpc")
	s.SetUnhealthy("pocket", "seed-one", "grpc", "broken")

	all := s.AllHealth()
	if len(all) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(all))
	}

	rpc, ok := all["pocket:seed-one:rpc"]
	if !ok {
		t.Fatal("missing rpc entry")
	}
	if !rpc.Healthy {
		t.Fatal("rpc should be healthy")
	}

	grpc, ok := all["pocket:seed-one:grpc"]
	if !ok {
		t.Fatal("missing grpc entry")
	}
	if grpc.Healthy {
		t.Fatal("grpc should be unhealthy")
	}
	if grpc.LastError != "broken" {
		t.Fatalf("expected 'broken', got %q", grpc.LastError)
	}
}

func TestHealthStore_AllHealth_Empty(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	all := s.AllHealth()
	if len(all) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(all))
	}
}

func TestHealthStore_GetHealth_ReturnsCopy(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	s.SetHealthy("pocket", "seed-one", "rpc")
	h := s.GetHealth("pocket", "seed-one", "rpc")

	// Mutate the copy — original should be unaffected.
	h.Healthy = false
	h.LastError = "mutated"

	if !s.IsHealthy("pocket", "seed-one", "rpc") {
		t.Fatal("original should still be healthy after copy mutation")
	}
}

func TestHealthStore_Clear(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	s.SetHealthy("pocket", "seed-one", "rpc")
	s.SetHealthy("pocket", "seed-two", "rpc")
	s.Clear()

	if s.IsHealthy("pocket", "seed-one", "rpc") {
		t.Fatal("expected unhealthy after Clear")
	}
	all := s.AllHealth()
	if len(all) != 0 {
		t.Fatalf("expected 0 entries after Clear, got %d", len(all))
	}
}

func TestHealthStore_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			s.SetHealthy("pocket", "seed-one", "rpc")
		}()
		go func() {
			defer wg.Done()
			s.SetUnhealthy("pocket", "seed-one", "rpc", "error")
		}()
		go func() {
			defer wg.Done()
			_ = s.IsHealthy("pocket", "seed-one", "rpc")
			_ = s.HealthyNodes("pocket", "rpc")
			_ = s.AllHealth()
		}()
	}
	wg.Wait()

	// Should not panic or race — final state is indeterminate but valid.
	h := s.GetHealth("pocket", "seed-one", "rpc")
	if h == nil {
		t.Fatal("expected non-nil health after concurrent access")
	}
}

// nodeAvailable returns sauron_node_available for the labels and whether that
// series is exported at all. It only collects: GetMetricWithLabelValues would
// create the series it is asked about.
func nodeAvailable(t *testing.T, network, node, protocol string) (float64, bool) {
	t.Helper()
	ch := make(chan prometheus.Metric, 256)
	go func() { metrics.NodeAvailable.Collect(ch); close(ch) }()
	value, exported := 0.0, false
	for c := range ch {
		var m dto.Metric
		if err := c.Write(&m); err != nil {
			t.Fatalf("write metric: %v", err)
		}
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["network"] == network && labels["node"] == node && labels["type"] == protocol {
			value, exported = m.GetGauge().GetValue(), true
		}
	}
	return value, exported
}

// Every protocol's routability is exported as sauron_node_available{type}, as
// V1 did per protocol: before, only the height-check protocol was, so a node
// whose REST or gRPC was down while its RPC answered raised no alert.
func TestHealthStore_ExportsAvailabilityPerProtocol(t *testing.T) {
	t.Parallel()

	s := NewHealthStore()
	const network = "avail-net"
	s.SetHealthy(network, "node-1", "rest")
	s.SetUnhealthy(network, "node-1", "grpc", "gRPC health query: unavailable")
	s.SetHealthy(network, "node-1", "rpc")

	for _, tt := range []struct {
		protocol string
		want     float64
	}{{"rest", 1}, {"grpc", 0}, {"rpc", 1}} {
		got, exported := nodeAvailable(t, network, "node-1", tt.protocol)
		if !exported || got != tt.want {
			t.Errorf("sauron_node_available{type=%q}: got %v exported=%v, want %v", tt.protocol, got, exported, tt.want)
		}
	}

	// A breaker trip goes through SetUnhealthy too, and recovers with the
	// next passing check.
	s.SetUnhealthy(network, "node-1", "rest", "circuit breaker tripped")
	if got, _ := nodeAvailable(t, network, "node-1", "rest"); got != 0 {
		t.Errorf("after breaker trip: rest = %v, want 0", got)
	}
	s.SetHealthy(network, "node-1", "rest")
	if got, _ := nodeAvailable(t, network, "node-1", "rest"); got != 1 {
		t.Errorf("after recovery: rest = %v, want 1", got)
	}
}

// External endpoints (node "ext:<url>") are not exported: a URL is an
// unbounded label value. WebSocket has its own gauge.
func TestHealthStore_DoesNotExportExternalsOrWebSocket(t *testing.T) {
	t.Parallel()

	s := NewHealthStore()
	const network = "avail-skip-net"
	s.SetUnhealthy(network, "ext:https://ring.example.com:443", "grpc", "breaker")
	s.SetHealthy(network, "node-1", "websocket")
	if _, exported := nodeAvailable(t, network, "ext:https://ring.example.com:443", "grpc"); exported {
		t.Error("external endpoint exported as a node series")
	}
	if _, exported := nodeAvailable(t, network, "node-1", "websocket"); exported {
		t.Error("websocket exported as sauron_node_available; it has sauron_node_websocket_available")
	}
}
