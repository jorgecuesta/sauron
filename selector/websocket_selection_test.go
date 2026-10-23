package selector

import (
	"testing"
	"time"

	"go.uber.org/zap"

	"sauron/storage"
)

// newWSSelector builds a selector over createTestConfig's network "pocket"
// (height check over rpc).
func newWSSelector(t *testing.T) (*Selector, *storage.HeightStore, *storage.ExternalEndpointStore) {
	t.Helper()
	heights := storage.NewHeightStore()
	endpoints := storage.NewExternalEndpointStore(zap.NewNop())
	return NewSelector(heights, endpoints, createTestConfig(t, 2), zap.NewNop()), heights, endpoints
}

func TestGetBestWebSocketNode_SkipsNodesWithoutWebSocket(t *testing.T) {
	t.Parallel()
	sel, heights, _ := newWSSelector(t)
	heights.Update("pocket", "node-1", "rpc", 100, time.Millisecond, "internal")
	heights.Update("pocket", "node-2", "rpc", 100, time.Millisecond, "internal")
	heights.UpdateWebSocketAvailability("pocket", "node-2", "rpc", true)

	for i := 0; i < 10; i++ {
		m, name, decision := sel.GetBestWebSocketNode("pocket", "rpc")
		if m == nil || name != "node-2" {
			t.Fatalf("call %d: selected %q, want node-2 (the only WebSocket node)", i, name)
		}
		if !m.WebSocketAvailable {
			t.Errorf("call %d: returned metrics have WebSocketAvailable=false", i)
		}
		if decision.Candidates != 1 || decision.Reason != "only_available" {
			t.Errorf("call %d: decision = %+v, want 1 candidate, only_available", i, decision)
		}
	}
}

func TestGetBestNode_UnaffectedByWebSocketAvailability(t *testing.T) {
	t.Parallel()
	sel, heights, _ := newWSSelector(t)
	heights.Update("pocket", "node-1", "rpc", 100, time.Millisecond, "internal")
	heights.Update("pocket", "node-2", "rpc", 100, time.Millisecond, "internal")
	heights.UpdateWebSocketAvailability("pocket", "node-2", "rpc", true)

	seen := map[string]int{}
	for i := 0; i < 10; i++ {
		_, name, _ := sel.GetBestNode("pocket", "rpc")
		seen[name]++
	}
	if seen["node-1"] != 5 || seen["node-2"] != 5 {
		t.Errorf("plain requests distribution = %v, want 5/5 over both nodes", seen)
	}
}

func TestGetBestWebSocketNode_PrefersLaggingWebSocketNodeOverNone(t *testing.T) {
	t.Parallel()
	sel, heights, _ := newWSSelector(t)
	heights.Update("pocket", "node-1", "rpc", 100, time.Millisecond, "internal")
	heights.Update("pocket", "node-2", "rpc", 99, time.Millisecond, "internal")
	heights.UpdateWebSocketAvailability("pocket", "node-2", "rpc", true)

	m, name, decision := sel.GetBestWebSocketNode("pocket", "rpc")
	if name != "node-2" || m == nil || m.Height != 99 {
		t.Fatalf("selected %q (metrics %+v), want node-2 at 99", name, m)
	}
	if decision.MaxHeight != 99 {
		t.Errorf("decision.MaxHeight = %d, want 99 (max among WebSocket nodes)", decision.MaxHeight)
	}
}

func TestGetBestWebSocketNode_NoWebSocketNodes(t *testing.T) {
	t.Parallel()
	sel, heights, _ := newWSSelector(t)
	heights.Update("pocket", "node-1", "rpc", 100, time.Millisecond, "internal")
	heights.Update("pocket", "node-2", "rpc", 100, time.Millisecond, "internal")
	heights.UpdateWebSocketAvailability("pocket", "node-2", "rpc", false)

	m, name, decision := sel.GetBestWebSocketNode("pocket", "rpc")
	if m != nil || name != "" || decision != nil {
		t.Errorf("got (%+v, %q, %+v), want no node", m, name, decision)
	}
}

func TestGetBestWebSocketNode_EmptyNetwork(t *testing.T) {
	t.Parallel()
	sel, _, _ := newWSSelector(t)
	if m, name, _ := sel.GetBestWebSocketNode("pocket", "rpc"); m != nil || name != "" {
		t.Errorf("got (%+v, %q), want no node", m, name)
	}
	if m, name, _ := sel.GetBestWebSocketNode("unknown-net", "rpc"); m != nil || name != "" {
		t.Errorf("unknown network: got (%+v, %q), want no node", m, name)
	}
}

func TestGetBestWebSocketNode_RoundRobinAmongWebSocketNodes(t *testing.T) {
	t.Parallel()
	sel, heights, _ := newWSSelector(t)
	for _, n := range []string{"node-1", "node-2", "node-3"} {
		heights.Update("pocket", n, "rpc", 100, time.Millisecond, "internal")
	}
	heights.UpdateWebSocketAvailability("pocket", "node-1", "rpc", true)
	heights.UpdateWebSocketAvailability("pocket", "node-3", "rpc", true)

	seen := map[string]int{}
	for i := 0; i < 10; i++ {
		_, name, decision := sel.GetBestWebSocketNode("pocket", "rpc")
		seen[name]++
		if decision == nil || decision.Reason != "round_robin" {
			t.Fatalf("call %d: decision = %+v, want reason round_robin", i, decision)
		}
	}
	if seen["node-1"] != 5 || seen["node-3"] != 5 || seen["node-2"] != 0 {
		t.Errorf("distribution = %v, want node-1 and node-3 5 each, node-2 never", seen)
	}
}

// Plain and WebSocket selections keep separate round-robin counters, so
// interleaving them does not skew either distribution.
func TestGetBestWebSocketNode_SeparateRoundRobinCounter(t *testing.T) {
	t.Parallel()
	sel, heights, _ := newWSSelector(t)
	heights.Update("pocket", "node-1", "rpc", 100, time.Millisecond, "internal")
	heights.Update("pocket", "node-2", "rpc", 100, time.Millisecond, "internal")
	heights.UpdateWebSocketAvailability("pocket", "node-1", "rpc", true)
	heights.UpdateWebSocketAvailability("pocket", "node-2", "rpc", true)

	seen := map[string]int{}
	for i := 0; i < 10; i++ {
		_, name, _ := sel.GetBestWebSocketNode("pocket", "rpc")
		seen[name]++
		sel.GetBestNode("pocket", "rpc")
	}
	if seen["node-1"] != 5 || seen["node-2"] != 5 {
		t.Errorf("WebSocket distribution with interleaved plain calls = %v, want 5/5", seen)
	}
}

func TestGetBestWebSocketNode_HealthFilterStillApplies(t *testing.T) {
	t.Parallel()
	sel, heights, _ := newWSSelector(t)
	health := storage.NewHealthStore()
	sel.SetHealthStore(health)
	heights.Update("pocket", "node-1", "rpc", 100, time.Millisecond, "internal")
	heights.Update("pocket", "node-2", "rpc", 100, time.Millisecond, "internal")
	heights.UpdateWebSocketAvailability("pocket", "node-1", "rpc", true)
	heights.UpdateWebSocketAvailability("pocket", "node-2", "rpc", true)
	health.SetHealthy("pocket", "node-1", "rpc")
	health.SetUnhealthy("pocket", "node-2", "rpc", "connection refused")

	for i := 0; i < 4; i++ {
		if _, name, _ := sel.GetBestWebSocketNode("pocket", "rpc"); name != "node-1" {
			t.Fatalf("call %d: selected %q, want node-1 (node-2 is unhealthy)", i, name)
		}
	}
}

// With no internal WebSocket node, a WebSocket-capable external is used even
// when it is not ahead by the failover threshold; one without WebSocket never is.
func TestGetBestWebSocketNode_Externals(t *testing.T) {
	t.Parallel()

	const url = "https://ext1.example.com:26657"
	tests := []struct {
		name     string
		extWS    bool
		wantNode string
	}{
		{name: "external with WebSocket is used", extWS: true, wantNode: "ext:" + url},
		{name: "external without WebSocket is not", extWS: false, wantNode: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sel, heights, endpoints := newWSSelector(t)
			heights.Update("pocket", "node-1", "rpc", 100, time.Millisecond, "internal")
			endpoints.StoreAdvertised("partner", "https://ring.example.com", "pocket", "rpc", url)
			endpoints.MarkValidated("partner", "https://ring.example.com", "pocket", "rpc", url, 100, time.Millisecond)
			endpoints.UpdateWebSocketAvailability("partner", "https://ring.example.com", "pocket", "rpc", url, tt.extWS)

			_, name, _ := sel.GetBestWebSocketNode("pocket", "rpc")
			if name != tt.wantNode {
				t.Errorf("selected %q, want %q", name, tt.wantNode)
			}
			// Plain selection keeps the threshold rule: the external is not ahead.
			if _, plain, _ := sel.GetBestNode("pocket", "rpc"); plain != "node-1" {
				t.Errorf("plain selection = %q, want node-1", plain)
			}
		})
	}
}
