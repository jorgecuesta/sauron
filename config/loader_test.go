package config

import (
	"os"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// writeMinimalConfig creates a temp YAML config file and returns its path.
func writeMinimalConfig(t *testing.T) string {
	t.Helper()
	content := `
api: true
rpc: true
grpc: true
listen: ":3000"
external_failover_threshold: 2
timeouts:
  health_check: 5s
  proxy: 30s
networks:
  - name: pocket
    api_listen: ":8080"
    rpc_listen: ":8081"
    grpc_listen: ":8082"
internals:
  - name: node-1
    api: "https://node1.example.com"
    rpc: "https://node1.example.com:26657"
    grpc: "node1.example.com:9090"
    network: pocket
`
	f, err := os.CreateTemp("", "sauron-cfg-*.yaml")
	if err != nil {
		t.Fatalf("create temp config: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp config: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	return f.Name()
}

// TestGetReturnsSamePointerWhenUnchanged verifies that two consecutive Get() calls
// return the exact same pointer (no copy is made on each call).
func TestGetReturnsSamePointerWhenUnchanged(t *testing.T) {
	path := writeMinimalConfig(t)
	loader, err := NewLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}

	p1 := loader.Get()
	p2 := loader.Get()

	if p1 != p2 {
		t.Errorf("expected same pointer, got different: %p vs %p", p1, p2)
	}
}

// TestConcurrentGetIsSafe spawns 100 goroutines all calling Get() simultaneously
// and verifies none of them return nil (race detector will catch data races).
func TestConcurrentGetIsSafe(t *testing.T) {
	path := writeMinimalConfig(t)
	loader, err := NewLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}

	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan string, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			cfg := loader.Get()
			if cfg == nil {
				errs <- "Get() returned nil"
			}
		}()
	}

	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}

// TestGetReturnsNewConfigAfterReload verifies that after Store()-ing a new Config,
// Get() returns the new pointer rather than the original one.
func TestGetReturnsNewConfigAfterReload(t *testing.T) {
	path := writeMinimalConfig(t)
	loader, err := NewLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}

	original := loader.Get()

	newCfg := &Config{
		API:    true,
		Listen: ":9999",
	}
	loader.config.Store(newCfg)

	got := loader.Get()
	if got == original {
		t.Errorf("expected new pointer after Store, still got original")
	}
	if got != newCfg {
		t.Errorf("expected stored pointer %p, got %p", newCfg, got)
	}
}
