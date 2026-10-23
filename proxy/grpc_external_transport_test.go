package proxy

import (
	"testing"

	"go.uber.org/zap"

	"sauron/internal/testutil"
	"sauron/storage"
)

// The proxy must dial an external endpoint with the transport its validation
// used, not TLS for every non-internal node (issue #31).
func TestShouldUseInsecureForNode_External(t *testing.T) {
	t.Parallel()

	loader := testutil.NewMultiChainLoader(t, `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: grpc
        listen: ":8082"
internals:
  - name: plain-node
    network: pocket
    grpc_insecure: true
    endpoints:
      grpc: "plain-node:9090"
  - name: tls-node
    network: pocket
    grpc_insecure: false
    endpoints:
      grpc: "tls-node:443"
`)
	store := storage.NewExternalEndpointStore(zap.NewNop())
	for _, ep := range []struct {
		url      string
		insecure bool
	}{{"ext-plain:9090", true}, {"ext-tls:443", false}} {
		store.StoreAdvertised("ring-a", "https://ring-a:3000", "pocket", "grpc", ep.url)
		store.SetGRPCTransport("ring-a", "https://ring-a:3000", "pocket", "grpc", ep.url, ep.insecure)
	}
	store.StoreAdvertised("ring-a", "https://ring-a:3000", "pocket", "grpc", "ext-unvalidated:443")

	p := NewGRPCProxy(nil, loader, store, zap.NewNop(), "pocket", nil)
	t.Cleanup(func() { _ = p.Close() })

	tests := []struct {
		node string
		want bool
	}{
		{"plain-node", true},
		{"tls-node", false},
		{"ext:ext-plain:9090", true},
		{"ext:ext-tls:443", false},
		{"ext:ext-unvalidated:443", false}, // no validated transport: TLS
		{"unknown-node", false},
	}
	for _, tt := range tests {
		if got := p.shouldUseInsecureForNode(tt.node); got != tt.want {
			t.Errorf("shouldUseInsecureForNode(%q): got %v, want %v", tt.node, got, tt.want)
		}
	}
}
