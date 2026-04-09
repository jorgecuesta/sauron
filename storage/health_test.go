package storage

import (
	"sync"
	"testing"
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
