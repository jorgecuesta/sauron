package storage

import (
	"sync"
	"testing"
	"time"
)

func TestHeightStore_UpdateAndGetByNetwork(t *testing.T) {
	t.Parallel()
	s := NewHeightStore()

	s.Update("pocket", "node-1", "api", 100, 10*time.Millisecond, "internal")
	s.Update("pocket", "node-2", "api", 200, 20*time.Millisecond, "internal")
	s.Update("pocket", "node-3", "api", 150, 15*time.Millisecond, "internal")

	nodes := s.GetByNetwork("pocket", "api")
	if len(nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(nodes))
	}

	cases := map[string]int64{
		"node-1": 100,
		"node-2": 200,
		"node-3": 150,
	}
	for name, wantHeight := range cases {
		m, ok := nodes[name]
		if !ok {
			t.Errorf("missing node %s in results", name)
			continue
		}
		if m.Height != wantHeight {
			t.Errorf("node %s: expected height %d, got %d", name, wantHeight, m.Height)
		}
	}
}

func TestHeightStore_GetHighestHeight(t *testing.T) {
	t.Parallel()
	s := NewHeightStore()

	s.Update("pocket", "node-a", "rpc", 50, 5*time.Millisecond, "internal")
	s.Update("pocket", "node-b", "rpc", 300, 30*time.Millisecond, "internal")
	s.Update("pocket", "node-c", "rpc", 200, 20*time.Millisecond, "internal")

	got := s.GetHighestHeight("pocket", "rpc")
	if got != 300 {
		t.Errorf("expected highest height 300, got %d", got)
	}
}

func TestHeightStore_LatencyHistory(t *testing.T) {
	t.Parallel()
	s := NewHeightStore()

	// Insert 15 latency samples; only the last 10 should be kept.
	for i := 1; i <= 15; i++ {
		s.Update("pocket", "node-1", "api", int64(i*100), time.Duration(i)*time.Millisecond, "internal")
	}

	nodes := s.GetByNetwork("pocket", "api")
	m, ok := nodes["node-1"]
	if !ok {
		t.Fatal("node-1 not found")
	}

	if len(m.LatencyHistory) != LatencyHistorySize {
		t.Errorf("expected latency history size %d, got %d", LatencyHistorySize, len(m.LatencyHistory))
	}

	// The last LatencyHistorySize updates used latencies 6ms…15ms
	expectedFirst := time.Duration(6) * time.Millisecond
	if m.LatencyHistory[0] != expectedFirst {
		t.Errorf("expected first entry in window %v, got %v", expectedFirst, m.LatencyHistory[0])
	}
	expectedLast := time.Duration(15) * time.Millisecond
	if m.LatencyHistory[len(m.LatencyHistory)-1] != expectedLast {
		t.Errorf("expected last entry in window %v, got %v", expectedLast, m.LatencyHistory[len(m.LatencyHistory)-1])
	}
}

func TestHeightStore_AvgLatency(t *testing.T) {
	t.Parallel()
	s := NewHeightStore()

	// 3 samples: 10ms, 20ms, 30ms → average = 20ms
	s.Update("pocket", "node-1", "api", 100, 10*time.Millisecond, "internal")
	s.Update("pocket", "node-1", "api", 100, 20*time.Millisecond, "internal")
	s.Update("pocket", "node-1", "api", 100, 30*time.Millisecond, "internal")

	nodes := s.GetByNetwork("pocket", "api")
	m := nodes["node-1"]
	if m == nil {
		t.Fatal("node-1 not found")
	}

	expected := 20 * time.Millisecond
	if m.AvgLatency != expected {
		t.Errorf("expected avg latency %v, got %v", expected, m.AvgLatency)
	}
}

func TestHeightStore_UpdateWebSocketAvailability(t *testing.T) {
	t.Parallel()
	s := NewHeightStore()

	// The node must exist before updating WS availability
	s.Update("pocket", "node-1", "rpc", 100, 10*time.Millisecond, "internal")
	s.UpdateWebSocketAvailability("pocket", "node-1", "rpc", true)

	nodes := s.GetByNetwork("pocket", "rpc")
	m, ok := nodes["node-1"]
	if !ok {
		t.Fatal("node-1 not found")
	}
	if !m.WebSocketAvailable {
		t.Error("expected WebSocketAvailable=true, got false")
	}

	// Now mark unavailable
	s.UpdateWebSocketAvailability("pocket", "node-1", "rpc", false)
	nodes = s.GetByNetwork("pocket", "rpc")
	m = nodes["node-1"]
	if m.WebSocketAvailable {
		t.Error("expected WebSocketAvailable=false after update, got true")
	}
}

func TestHeightStore_FiltersCorrectly(t *testing.T) {
	t.Parallel()
	s := NewHeightStore()

	// Different networks
	s.Update("pocket", "node-1", "api", 100, 10*time.Millisecond, "internal")
	s.Update("pocket-beta", "node-1", "api", 200, 10*time.Millisecond, "internal")

	// Different endpoint types
	s.Update("pocket", "node-2", "rpc", 300, 10*time.Millisecond, "internal")

	pocketAPI := s.GetByNetwork("pocket", "api")
	if len(pocketAPI) != 1 {
		t.Errorf("expected 1 pocket/api node, got %d", len(pocketAPI))
	}
	if _, ok := pocketAPI["node-1"]; !ok {
		t.Error("expected node-1 in pocket/api")
	}

	betaAPI := s.GetByNetwork("pocket-beta", "api")
	if len(betaAPI) != 1 {
		t.Errorf("expected 1 pocket-beta/api node, got %d", len(betaAPI))
	}

	pocketRPC := s.GetByNetwork("pocket", "rpc")
	if len(pocketRPC) != 1 {
		t.Errorf("expected 1 pocket/rpc node, got %d", len(pocketRPC))
	}
	if _, ok := pocketRPC["node-2"]; !ok {
		t.Error("expected node-2 in pocket/rpc")
	}

	// No cross-contamination
	if _, ok := pocketAPI["node-2"]; ok {
		t.Error("node-2 (rpc) should not appear in api results")
	}
}

func TestHeightStore_ConcurrentUpdates(t *testing.T) {
	t.Parallel()
	s := NewHeightStore()

	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			nodeName := "node-" + string(rune('A'+i%26))
			s.Update("pocket", nodeName, "api", int64(i*10), time.Duration(i)*time.Millisecond, "internal")
			s.GetByNetwork("pocket", "api")
			s.GetHighestHeight("pocket", "api")
		}()
	}

	wg.Wait()
	// If we reach here without a panic or data race detected by -race, the test passes.
}
