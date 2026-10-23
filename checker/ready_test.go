package checker

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"sauron/adapter"
	"sauron/adapter/cosmos"
	"sauron/internal/testutil"
	"sauron/storage"
)

func waitReady(t *testing.T, sched *MultiChainScheduler) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !sched.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("scheduler not ready 10s after the first checks")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Readiness means the first checks have finished, not that a node is healthy:
// a node that fails its check still makes the scheduler ready (issue #29).
func TestScheduler_ReadyAfterFirstChecks_EvenWhenTheNodeFails(t *testing.T) {
	t.Parallel()

	sched, _, healthStore := testMultiChainSetup(t, "http://127.0.0.1:1")
	if sched.Ready() {
		t.Fatal("ready before any check ran")
	}
	sched.checkInternalNodes()
	waitReady(t, sched)
	if healthStore.IsHealthy("pocket", "seed-one", "rpc") {
		t.Fatal("dead node reported healthy")
	}
}

func TestScheduler_ReadyAfterFirstChecks_HealthyNode(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":-1,"result":{"response":{"data":"pocket","last_block_height":"976400"}}}`))
	}))
	t.Cleanup(srv.Close)

	sched, heightStore, _ := testMultiChainSetup(t, srv.URL)
	if sched.Ready() {
		t.Fatal("ready before any check ran")
	}
	sched.checkInternalNodes()
	waitReady(t, sched)
	if h := heightStore.GetHighestHeight("pocket", "rpc"); h != 976400 {
		t.Fatalf("height after ready: got %d, want 976400", h)
	}
}

// Once ready, the scheduler stays ready: a node added later and not yet
// checked must not flip every replica to unready.
func TestScheduler_ReadyLatches(t *testing.T) {
	t.Parallel()

	sched, _, _ := testMultiChainSetup(t, "http://127.0.0.1:1")
	sched.checkInternalNodes()
	waitReady(t, sched)
	sched.heightChecked.Clear()
	if !sched.Ready() {
		t.Fatal("readiness was reset after it had been reached")
	}
}

// With no internal nodes, readiness waits for the first query of the external
// rings; before #29 such a config was never ready.
func TestScheduler_ReadyExternalsOnly(t *testing.T) {
	t.Parallel()

	ring := httptest.NewServer(newStatusHandler(976400, map[string]string{}))
	t.Cleanup(ring.Close)

	loader := testutil.NewMultiChainLoader(t, `
listen: ":3000"
timeouts:
  health_check: 2s
  proxy: 10s
networks:
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: rest
        listen: ":18080"
externals:
  - name: ring-ready
    rings: ["`+ring.URL+`"]
`)
	registry := adapter.NewRegistry()
	if err := registry.Register(cosmos.New()); err != nil {
		t.Fatalf("register cosmos: %v", err)
	}
	sched := NewMultiChainScheduler(adapter.NewEngine(http.DefaultClient), registry, storage.NewHeightStore(), storage.NewHealthStore(), nil,
		storage.NewCache("", zap.NewNop()), storage.NewExternalEndpointStore(zap.NewNop()), nil, loader, newTestPool(), zap.NewNop())
	t.Cleanup(sched.extChecker.Close)

	if sched.Ready() {
		t.Fatal("ready before the rings were queried")
	}
	sched.checkExternalRings()
	waitReady(t, sched)
}
