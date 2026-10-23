package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// seriesWith counts the series of c whose labels include want.
func seriesWith(t *testing.T, c prometheus.Collector, want map[string]string) int {
	t.Helper()
	ch := make(chan prometheus.Metric, 256)
	c.Collect(ch)
	close(ch)
	n := 0
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("write metric: %v", err)
		}
		labels := map[string]string{}
		for _, l := range pb.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		match := true
		for k, v := range want {
			if labels[k] != v {
				match = false
			}
		}
		if match {
			n++
		}
	}
	return n
}

// #20: a node removed from the config stops being exported by the gauges and
// histograms that describe its current state; other nodes keep theirs.
func TestDeleteNodeSeries(t *testing.T) {
	NodeHeight.WithLabelValues("del-net", "gone", "rpc", "internal").Set(100)
	NodeHeight.WithLabelValues("del-net", "kept", "rpc", "internal").Set(101)
	NodeHeight.WithLabelValues("del-other", "gone", "rpc", "internal").Set(5)
	NodeAvailable.WithLabelValues("del-net", "gone", "rpc").Set(1)
	NodeWebSocketAvailable.WithLabelValues("del-net", "gone", "rpc").Set(1)
	NodeLatency.WithLabelValues("del-net", "gone", "rpc").Observe(0.1)

	DeleteNodeSeries("del-net", "gone")

	for name, c := range map[string]prometheus.Collector{
		"sauron_node_height": NodeHeight, "sauron_node_available": NodeAvailable,
		"sauron_node_websocket_available": NodeWebSocketAvailable, "sauron_node_latency_seconds": NodeLatency,
	} {
		if n := seriesWith(t, c, map[string]string{"network": "del-net", "node": "gone"}); n != 0 {
			t.Errorf("%s still exports %d series for the removed node", name, n)
		}
	}
	var kept dto.Metric
	_ = NodeHeight.WithLabelValues("del-net", "kept", "rpc", "internal").Write(&kept)
	if got := kept.GetGauge().GetValue(); got != 101 {
		t.Errorf("kept node height = %v, want 101", got)
	}
	if n := seriesWith(t, NodeHeight, map[string]string{"network": "del-other", "node": "gone"}); n != 1 {
		t.Errorf("same node name in another network: %d series, want 1 kept", n)
	}
}

func TestDeleteExternalSeries(t *testing.T) {
	ExternalRingAvailable.WithLabelValues("del-ext", "https://ring").Set(1)
	ExternalEndpointsWorking.WithLabelValues("pocket", "rest", "del-ext").Set(2)
	ExternalEndpointErrorCount.WithLabelValues("pocket", "rest", "del-ext").Set(3)
	ExternalEndpointsWorking.WithLabelValues("pocket", "rest", "kept-ext").Set(1)

	DeleteExternalSeries("del-ext")

	if n := seriesWith(t, ExternalRingAvailable, map[string]string{"ring_name": "del-ext"}); n != 0 {
		t.Errorf("ring availability still exported (%d)", n)
	}
	if n := seriesWith(t, ExternalEndpointsWorking, map[string]string{"ring_name": "del-ext"}); n != 0 {
		t.Errorf("endpoints working still exported (%d)", n)
	}
	if n := seriesWith(t, ExternalEndpointErrorCount, map[string]string{"ring_name": "del-ext"}); n != 0 {
		t.Errorf("endpoint error count still exported (%d)", n)
	}
	if n := seriesWith(t, ExternalEndpointsWorking, map[string]string{"ring_name": "kept-ext"}); n != 1 {
		t.Errorf("kept external lost its series (%d)", n)
	}
}

// CLAUDE.md: no high-cardinality labels, no URLs. A "url" label held the URLs
// external rings advertise: chosen by a remote party, so a broken or hostile
// ring could grow the series without bound. (ring_url comes from our own config
// and stays.) Every metric is checked, so a new url label fails here.
func TestNoURLLabels(t *testing.T) {
	// Give every vector a series so Gather reports its labels.
	ExternalEndpointErrorCount.WithLabelValues("pocket", "rest", "label-check").Set(0)
	ExternalEndpointProxyErrors.WithLabelValues("pocket", "rest", "label-check").Add(0)

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if !strings.HasPrefix(mf.GetName(), "sauron_") {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if strings.ToLower(l.GetName()) == "url" {
					t.Errorf("%s has label %q: URLs are unbounded; use the node or external name", mf.GetName(), l.GetName())
				}
			}
		}
	}
}
