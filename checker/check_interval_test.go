package checker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"sauron/adapter"
	"sauron/adapter/cosmos"
	"sauron/internal/testutil"
	"sauron/storage"
)

func TestSchedulerDue(t *testing.T) {
	t.Parallel()
	s := &MultiChainScheduler{lastRun: newLastRunMap()}
	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	steps := []struct {
		key      string
		interval time.Duration
		at       time.Duration
		want     bool
	}{
		{key: "a", interval: 3 * time.Second, at: 0, want: true},                        // first time: run
		{key: "a", interval: 3 * time.Second, at: time.Second, want: false},             // too early
		{key: "a", interval: 3 * time.Second, at: 2999 * time.Millisecond, want: false}, // just before
		{key: "a", interval: 3 * time.Second, at: 3 * time.Second, want: true},          // exactly due
		{key: "b", interval: 3 * time.Second, at: 3 * time.Second, want: true},          // keys are independent
		{key: "a", interval: time.Second, at: 4 * time.Second, want: true},              // a shorter interval applies at once
		{key: "a", interval: time.Minute, at: 5 * time.Second, want: false},             // a longer one too
		{key: "a", interval: time.Minute, at: -time.Second, want: false},                // clock going back never runs early
	}
	for i, st := range steps {
		if got := s.due(st.key, st.interval, t0.Add(st.at)); got != st.want {
			t.Errorf("step %d: due(%s, %v, t0%+v) = %v, want %v", i, st.key, st.interval, st.at, got, st.want)
		}
	}
}

// Overlapping ticks must not both run the same check.
func TestSchedulerDue_Concurrent(t *testing.T) {
	t.Parallel()
	s := &MultiChainScheduler{lastRun: newLastRunMap()}
	now := time.Now()
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.due("k", time.Second, now) {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := wins.Load(); got != 1 {
		t.Errorf("%d goroutines saw the check as due, want exactly 1", got)
	}
}

// countingNode serves a Cosmos node's RPC /abci_info and /status and REST
// block queries, counting height and archival checks.
type countingNode struct {
	height   atomic.Int32 // /abci_info, the RPC height check
	archival atomic.Int32
}

func (c *countingNode) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/abci_info":
			c.height.Add(1)
			_, _ = w.Write([]byte(`{"result":{"response":{"last_block_height":"100"}}}`))
		case "/status":
			_, _ = w.Write([]byte(`{"result":{"sync_info":{"latest_block_height":"100"}}}`))
		case "/cosmos/base/tendermint/v1beta1/blocks/1":
			c.archival.Add(1)
			_, _ = w.Write([]byte(`{"sdk_block":{"header":{"height":"1"}}}`))
		case "/cosmos/base/tendermint/v1beta1/blocks/latest":
			_, _ = w.Write([]byte(`{"sdk_block":{"header":{"height":"100"}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSchedulerTick_PerNetworkIntervals (#21): each network's height_check
// interval and archival check_interval decide how often its checks run.
// Ticks at t0..t0+3s: "fast" (height 1s, archival 2s) runs height every tick
// and archival at t0 and t0+2s; "slow" (height 3s) runs at t0 and t0+3s.
func TestSchedulerTick_PerNetworkIntervals(t *testing.T) {
	t.Parallel()

	var fast, slow countingNode
	fastSrv, slowSrv := fast.server(t), slow.server(t)

	yaml := `
listen: ":3000"
timeouts:
  health_check: 2s
  proxy: 60s
networks:
  - name: fast
    type: cosmos
    height_check:
      protocol: rpc
      interval: 1s
    endpoints:
      - protocol: rest
        listen: ":18080"
      - protocol: rpc
        listen: ":18081"
    archival:
      min_height: 1
      check_interval: 2s
  - name: slow
    type: cosmos
    height_check:
      protocol: rpc
      interval: 3s
    endpoints:
      - protocol: rest
        listen: ":18090"
      - protocol: rpc
        listen: ":18091"
internals:
  - name: fast-node
    network: fast
    endpoints:
      rest: "` + fastSrv.URL + `"
      rpc: "` + fastSrv.URL + `"
  - name: slow-node
    network: slow
    endpoints:
      rest: "` + slowSrv.URL + `"
      rpc: "` + slowSrv.URL + `"
`
	registry := adapter.NewRegistry()
	if err := registry.Register(cosmos.New()); err != nil {
		t.Fatalf("register cosmos: %v", err)
	}
	engine := adapter.NewEngine(&http.Client{Timeout: 2 * time.Second})
	sched := NewMultiChainScheduler(engine, registry, storage.NewHeightStore(), storage.NewHealthStore(),
		storage.NewArchivalStore(), storage.NewCache("", zap.NewNop()), storage.NewExternalEndpointStore(zap.NewNop()),
		nil, testutil.NewMultiChainLoader(t, yaml), newTestPool(), zap.NewNop())

	t0 := time.Now()
	for i := 0; i < 4; i++ {
		sched.tick(t0.Add(time.Duration(i) * time.Second))
	}
	drainTestPool(sched.pool)

	fs, ss := fast.height.Load(), slow.height.Load()
	if ss == 0 {
		t.Fatal("slow network never checked")
	}
	if fs != 2*ss {
		t.Errorf("height checks: fast %d, slow %d; want fast = 2 x slow (4 rounds vs 2)", fs, ss)
	}
	if got := fast.archival.Load(); got != 2 {
		t.Errorf("fast archival checks = %d, want 2 (t0 and t0+2s at check_interval 2s)", got)
	}
	if got := slow.archival.Load(); got != 0 {
		t.Errorf("slow archival checks = %d, want 0 (no archival config)", got)
	}
}

// TestOracleChecker_CheckDue: each network's oracles run on its own interval.
func TestOracleChecker_CheckDue(t *testing.T) {
	t.Parallel()

	counting := func(hits *atomic.Int32) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			_, _ = w.Write([]byte(`{"result":"0x64"}`))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	var fastHits, slowHits atomic.Int32
	fastSrv, slowSrv := counting(&fastHits), counting(&slowHits)

	oracleStore := storage.NewOracleStore()
	oc := NewOracleChecker(adapter.NewEngine(&http.Client{Timeout: 2 * time.Second}), oracleStore, zap.NewNop())
	check := adapter.CheckConfig{Method: "POST", URLPath: "/", ResponsePath: ".result", Protocol: "jsonrpc"}
	oc.AddConfig(OracleConfig{Network: "fast", Interval: time.Second, Oracles: []OracleEndpoint{{URL: fastSrv.URL, Check: check}}})
	oc.AddConfig(OracleConfig{Network: "slow", Interval: 3 * time.Second, Oracles: []OracleEndpoint{{URL: slowSrv.URL, Check: check}}})

	s := &MultiChainScheduler{lastRun: newLastRunMap()}
	t0 := time.Now()
	for i := 0; i < 4; i++ {
		now := t0.Add(time.Duration(i) * time.Second)
		oc.CheckDue(context.Background(), 2*time.Second, func(network string, interval time.Duration) bool {
			return s.due("oracle:"+network, interval, now)
		})
	}
	if got := fastHits.Load(); got != 4 {
		t.Errorf("fast oracle queried %d times, want 4", got)
	}
	if got := slowHits.Load(); got != 2 {
		t.Errorf("slow oracle queried %d times, want 2", got)
	}
	if h, ok := oracleStore.Get("slow"); !ok || h != 100 {
		t.Errorf("slow oracle height = %d, %v; want 100", h, ok)
	}
}

// TestScheduler_StartHonoursHeightInterval: Start wires the tick, so a network
// with height_check.interval 1s is checked about every second (the former
// fixed cron ran at most once in this window).
func TestScheduler_StartHonoursHeightInterval(t *testing.T) {
	t.Parallel()

	var node countingNode
	srv := node.server(t)
	yaml := `
listen: ":3000"
timeouts:
  health_check: 2s
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    height_check:
      protocol: rpc
      interval: 1s
    endpoints:
      - protocol: rpc
        listen: ":18081"
internals:
  - name: seed-one
    network: pocket
    endpoints:
      rpc: "` + srv.URL + `"
`
	registry := adapter.NewRegistry()
	if err := registry.Register(cosmos.New()); err != nil {
		t.Fatalf("register cosmos: %v", err)
	}
	heightStore := storage.NewHeightStore()
	sched := NewMultiChainScheduler(adapter.NewEngine(&http.Client{Timeout: 2 * time.Second}), registry, heightStore,
		storage.NewHealthStore(), nil, storage.NewCache("", zap.NewNop()), storage.NewExternalEndpointStore(zap.NewNop()),
		nil, testutil.NewMultiChainLoader(t, yaml), newTestPool(), zap.NewNop())
	if err := sched.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sched.Stop()

	seen := map[time.Time]bool{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(seen) < 3 {
		if m, ok := heightStore.GetByNetwork("pocket", "rpc")["seed-one"]; ok {
			seen[m.Timestamp] = true
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(seen) < 3 {
		t.Errorf("height checked %d distinct times in 5s at interval 1s, want at least 3", len(seen))
	}
}

// Archival status is logged at Info only when it changes: with a short
// check_interval, an unchanged result every few seconds must stay at Debug.
func TestCheckArchivalStatus_InfoOnlyOnChange(t *testing.T) {
	t.Parallel()

	var archival atomic.Bool
	archival.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !archival.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"sdk_block":{"header":{"height":"1"}}}`))
	}))
	defer srv.Close()

	sched, archivalStore := testMultiChainSetupWithArchival(t, srv.URL)
	core, logs := observer.New(zapcore.InfoLevel)
	sched.logger = zap.New(core)
	run := func() {
		sched.pool = newTestPool()
		sched.checkArchivalStatus()
		drainTestPool(sched.pool)
	}

	run()
	run()
	run()
	if n := logs.FilterMessage("Node confirmed archival").Len(); n != 1 {
		t.Errorf("Info 'Node confirmed archival' logged %d times over 3 identical checks, want 1", n)
	}
	archival.Store(false)
	run()
	run()
	if n := logs.FilterMessage("Node is not archival").Len(); n != 1 {
		t.Errorf("Info 'Node is not archival' logged %d times over 2 identical checks, want 1", n)
	}
	if archivalStore.IsArchival("pocket", "seed-one") {
		t.Error("node still archival after the block became unavailable")
	}
}
