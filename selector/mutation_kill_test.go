package selector

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"

	"sauron/internal/testutil"
	"sauron/metrics"
	"sauron/storage"
)

// counterValue reads the current value of one labelled child of a counter.
// The counters are process-global, so callers compare deltas and use a
// network name no other test uses.
func counterValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%v): %v", labels, err)
	}
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

// Pins: round-robin runs over candidates sorted ASCENDING by name, so with two
// equal-height nodes the first selection (counter 1 -> index 1) is the
// alphabetically last one, then it alternates. A reversed or unsorted order
// would change which node serves which request.
func TestMutationKill_RoundRobinOrderIsAscendingByName(t *testing.T) {
	t.Parallel()

	heights := storage.NewHeightStore()
	// Inserted in non-sorted order on purpose.
	heights.Update("pocket", "b-node", "rpc", 100, 10*time.Millisecond, "internal")
	heights.Update("pocket", "a-node", "rpc", 100, 10*time.Millisecond, "internal")
	sel := NewSelector(heights, nil, createTestConfig(t, 2), zap.NewNop())

	want := []string{"b-node", "a-node", "b-node", "a-node"}
	for i, w := range want {
		_, name, decision := sel.GetBestNode("pocket", "rest")
		if name != w {
			t.Fatalf("call %d: selected %q, want %q", i+1, name, w)
		}
		if decision.Reason != "round_robin" {
			t.Errorf("call %d: Reason = %q, want round_robin", i+1, decision.Reason)
		}
		if decision.Candidates != 2 {
			t.Errorf("call %d: Candidates = %d, want 2", i+1, decision.Candidates)
		}
		if decision.MaxHeight != 100 {
			t.Errorf("call %d: MaxHeight = %d, want 100", i+1, decision.MaxHeight)
		}
	}
}

// Pins: an external node's name is "ext:"+URL and GetEndpointURL strips the
// prefix to give back the URL, for a node that is not an internal. Also pins
// that a name that is only the prefix, or merely starts with "ext" and no
// colon, yields "" rather than a slice of itself.
func TestMutationKill_GetEndpointURL_ExternalPrefix(t *testing.T) {
	t.Parallel()

	sel := NewSelector(storage.NewHeightStore(), nil, createTestConfig(t, 2), zap.NewNop())

	tests := []struct {
		name     string
		nodeName string
		want     string
	}{
		{name: "external with https url", nodeName: "ext:https://ext1.example.com", want: "https://ext1.example.com"},
		{name: "external with one-char url", nodeName: "ext:x", want: "x"},
		{name: "prefix only", nodeName: "ext:", want: ""},
		{name: "no colon", nodeName: "extx.example.com", want: ""},
		{name: "unknown internal name", nodeName: "node-unknown", want: ""},
		{name: "empty name", nodeName: "", want: ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sel.GetEndpointURL(tt.nodeName, "rest"); got != tt.want {
				t.Errorf("GetEndpointURL(%q, rest) = %q, want %q", tt.nodeName, got, tt.want)
			}
		})
	}
}

// Pins: a height-store node recorded with an EMPTY source is counted in
// sauron_routing_selections_total under node_source="internal", not under "".
func TestMutationKill_SelectionMetric_EmptySourceCountsAsInternal(t *testing.T) {
	const network = "mutkill-src-empty"
	heights := storage.NewHeightStore()
	heights.Update(network, "node-1", "rpc", 100, 10*time.Millisecond, "")
	sel := NewSelector(heights, nil, createTestConfig(t, 2), zap.NewNop())

	internalBefore := counterValue(t, metrics.RoutingSelections, network, "rest", "internal", "only_available")
	emptyBefore := counterValue(t, metrics.RoutingSelections, network, "rest", "", "only_available")

	_, name, decision := sel.GetBestNode(network, "rest")
	if name != "node-1" {
		t.Fatalf("selected %q, want node-1", name)
	}
	if decision.Reason != "only_available" {
		t.Fatalf("Reason = %q, want only_available", decision.Reason)
	}

	if d := counterValue(t, metrics.RoutingSelections, network, "rest", "internal", "only_available") - internalBefore; d != 1 {
		t.Errorf("internal counter delta = %v, want 1", d)
	}
	if d := counterValue(t, metrics.RoutingSelections, network, "rest", "", "only_available") - emptyBefore; d != 0 {
		t.Errorf("empty-source counter delta = %v, want 0", d)
	}
}

// Pins: an external winner is counted under node_source="external" (its own
// Source), never relabelled "internal".
func TestMutationKill_SelectionMetric_ExternalCountsAsExternal(t *testing.T) {
	const network = "mutkill-src-external"
	logger := zap.NewNop()
	heights := storage.NewHeightStore()
	endpoints := storage.NewExternalEndpointStore(logger)
	endpoints.StoreAdvertised("ext-1", "https://ring.example.com", network, "rest", "https://ext1.example.com")
	endpoints.MarkValidated("ext-1", "https://ring.example.com", network, "rest", "https://ext1.example.com", 100, 5*time.Millisecond)
	sel := NewSelector(heights, endpoints, createTestConfig(t, 2), logger)

	externalBefore := counterValue(t, metrics.RoutingSelections, network, "rest", "external", "only_available")
	internalBefore := counterValue(t, metrics.RoutingSelections, network, "rest", "internal", "only_available")

	_, name, decision := sel.GetBestNode(network, "rest")
	if name != "ext:https://ext1.example.com" {
		t.Fatalf("selected %q, want ext:https://ext1.example.com", name)
	}
	if decision.Reason != "only_available" {
		t.Fatalf("Reason = %q, want only_available", decision.Reason)
	}

	if d := counterValue(t, metrics.RoutingSelections, network, "rest", "external", "only_available") - externalBefore; d != 1 {
		t.Errorf("external counter delta = %v, want 1", d)
	}
	if d := counterValue(t, metrics.RoutingSelections, network, "rest", "internal", "only_available") - internalBefore; d != 0 {
		t.Errorf("internal counter delta = %v, want 0", d)
	}
}

// Pins: a custom-type network with no height_check protocol and NO endpoints
// falls back to "rpc" instead of indexing Endpoints[0]. The validator rejects
// such a config at load, so the test empties the endpoints of its own private
// loader's config after loading; the guard is defensive and this is the only
// way to reach it.
func TestMutationKill_HeightCheckProtocol_CustomWithoutEndpointsFallsBackToRPC(t *testing.T) {
	t.Parallel()

	loader := testutil.NewMultiChainLoader(t, `listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: custom-empty
    type: custom
    height_check:
      interval: 30s
      method: POST
      url_path: /graphql
      headers:
        Content-Type: application/json
      body: '{"query":"{ height }"}'
      response_path: ".data.height"
      response_format: integer
    endpoints:
      - protocol: http
        listen: ":9500"
internals:
  - name: node-x
    network: custom-empty
    endpoints:
      http: "http://localhost:9500"
`)
	net := loader.Get().FindNetwork("custom-empty")
	if net == nil {
		t.Fatal("network custom-empty not found")
	}
	sel := NewSelector(storage.NewHeightStore(), nil, loader, zap.NewNop())

	// Sanity: with the endpoint present the first endpoint's protocol wins.
	if got := sel.heightCheckProtocol("custom-empty"); got != "http" {
		t.Fatalf("with endpoint: heightCheckProtocol = %q, want http", got)
	}

	net.Endpoints = nil
	if got := sel.heightCheckProtocol("custom-empty"); got != "rpc" {
		t.Errorf("without endpoints: heightCheckProtocol = %q, want rpc", got)
	}
}
