// Package testutil provides shared test helpers for creating V2 config loaders.
// This eliminates the boilerplate of writing temp YAML files across test packages.
package testutil

import (
	"os"
	"testing"

	"sauron/config"

	"go.uber.org/zap"
)

// NewMultiChainLoader writes yaml to a temp file and returns a *config.MultiChainLoader.
// The temp file is cleaned up when the test completes.
func NewMultiChainLoader(t *testing.T, yaml string) *config.MultiChainLoader {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "sauron-test-*.yaml")
	if err != nil {
		t.Fatalf("testutil.NewMultiChainLoader: CreateTemp: %v", err)
	}
	if _, err := f.WriteString(yaml); err != nil {
		t.Fatalf("testutil.NewMultiChainLoader: WriteString: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("testutil.NewMultiChainLoader: Close: %v", err)
	}

	loader, err := config.NewMultiChainLoader(f.Name(), zap.NewNop())
	if err != nil {
		t.Fatalf("testutil.NewMultiChainLoader: NewMultiChainLoader: %v", err)
	}
	return loader
}

// MinimalV2YAML returns a valid minimal V2 config with one Cosmos network and one node.
// Suitable for tests that need "any valid config" without specific requirements.
const MinimalV2YAML = `listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rest
        listen: ":8080"
      - protocol: rpc
        listen: ":8081"
internals:
  - name: node-1
    network: pocket
    endpoints:
      rest: "http://localhost:1317"
      rpc: "http://localhost:26657"
`
