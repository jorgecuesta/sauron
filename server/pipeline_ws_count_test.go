package server

import (
	"testing"
)

// A WebSocket session counts in sauron_node_requests_total as soon as it is
// upgraded, not when it ends: a session can last hours, and before this it
// was invisible (and the matrix test raced the count of the last session).
func TestPipeline_WebSocketCountedAtUpgrade(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real servers")
	}
	t.Parallel()

	p := startPipeline(t)
	total := func() float64 {
		return p.metricValue(t, "sauron_node_requests_total", map[string]string{"node": p.a, "type": "rpc", "method": "WEBSOCKET"}) +
			p.metricValue(t, "sauron_node_requests_total", map[string]string{"node": p.b, "type": "rpc", "method": "WEBSOCKET"})
	}
	before := total()

	c, _, err := wsDialer.Dial("ws://"+p.rpc+"/websocket", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	// The session is still open; the count must already include it.
	if d := total() - before; d != 1 {
		t.Fatalf("WEBSOCKET requests while the session is open: +%v, want +1", d)
	}
}
