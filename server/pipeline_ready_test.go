package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"sauron/internal/testtls"
)

func startConfig(t *testing.T, yaml string) {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Shutdown)
}

func waitReadyCode(t *testing.T, statusAddr string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	last := 0
	for time.Now().Before(deadline) {
		if resp, err := http.Get("http://" + statusAddr + "/ready"); err == nil {
			last = resp.StatusCode
			_ = resp.Body.Close()
			if last == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("/ready not 200 within %s (last %d)", within, last)
}

// Every node down: /ready reaches 200 once the first checks have run (it does
// not follow node health, so replicas sharing these nodes stay in the Service)
// and requests get a fast 503 (issue #29).
func TestPipeline_ReadyWithEveryNodeDown(t *testing.T) {
	t.Parallel()

	status, rest := freeAddr(t), freeAddr(t)
	name := fmt.Sprintf("ready-down-%d", pipelineSeq.Add(1))
	startConfig(t, fmt.Sprintf(`
listen: "%s"
timeouts:
  health_check: 1s
  proxy: 5s
networks:
  - name: pocket
    type: cosmos
    height_check:
      interval: 1s
    endpoints:
      - protocol: rest
        listen: "%s"
internals:
  - name: %s
    network: pocket
    endpoints:
      rest: "http://127.0.0.1:1"
      rpc: "http://127.0.0.1:1"
`, status, rest, name))

	waitReadyCode(t, status, 10*time.Second)
	resp, err := http.Get("http://" + rest + "/cosmos/base/tendermint/v1beta1/blocks/latest")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("request with every node down: got %d, want 503", resp.StatusCode)
	}
}

// A config with only external rings becomes ready once the rings were queried;
// before #29 it answered 503 "no internal nodes configured" forever.
func TestPipeline_ReadyExternalsOnly(t *testing.T) {
	t.Parallel()

	backend, stop, err := testtls.StartBlockServer(nil, 976400)
	if err != nil {
		t.Fatalf("start backend: %v", err)
	}
	t.Cleanup(stop)
	ring := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/status") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"height": 976400, "endpoints": map[string]string{"grpc": backend}})
	}))
	t.Cleanup(ring.Close)

	status, grpcAddr := freeAddr(t), freeAddr(t)
	ringName := fmt.Sprintf("ready-ext-%d", pipelineSeq.Add(1))
	startConfig(t, fmt.Sprintf(externalOnlyYAML, status, grpcAddr, ringName, ring.URL))
	waitReadyCode(t, status, 15*time.Second)
}
