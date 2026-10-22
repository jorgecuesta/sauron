package metrics

import "github.com/prometheus/client_golang/prometheus"

// DeleteNodeSeries stops exporting the gauges and histograms that describe an
// internal node's current state, once the node is gone from the config, so no
// dashboard keeps showing its last value. Counters are kept: they are history.
func DeleteNodeSeries(network, node string) {
	labels := prometheus.Labels{"network": network, "node": node}
	NodeHeight.DeletePartialMatch(labels)
	NodeLatency.DeletePartialMatch(labels)
	NodeAvailable.DeletePartialMatch(labels)
	NodeWebSocketAvailable.DeletePartialMatch(labels)
}

// DeleteExternalSeries stops exporting the state gauges of a removed external
// ring and of its endpoints (by URL). Counters are kept.
func DeleteExternalSeries(externalName string, endpointURLs []string) {
	ring := prometheus.Labels{"ring_name": externalName}
	ExternalRingAvailable.DeletePartialMatch(ring)
	ExternalEndpointsTracked.DeletePartialMatch(ring)
	ExternalEndpointsValidated.DeletePartialMatch(ring)
	ExternalEndpointsWorking.DeletePartialMatch(ring)
	for _, url := range endpointURLs {
		ExternalEndpointErrorCount.DeletePartialMatch(prometheus.Labels{"url": url})
	}
}
