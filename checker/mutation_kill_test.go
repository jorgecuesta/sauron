package checker

// Tests in this file pin behaviours that mutation testing (gremlins, issue #19)
// showed to be unobserved by the rest of the suite. Each test names the
// behaviour it pins; the mutated line is cited so a future change that breaks
// one can find its reason.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc/keepalive"

	"sauron/adapter"
	"sauron/config"
	"sauron/internal/grpcutil"
	"sauron/internal/testutil"
	"sauron/storage"
)

// deadAddr returns a host:port nothing listens on (connection refused at once).
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestOracleCheckDue_PassesDefaultIntervalWhenUnset pins oracle_checker.go
// `interval <= 0`: a network whose sync check_interval is unset (0) or negative
// is gated on config.DefaultCheckInterval, not on 0 (which would run it every
// tick), and a positive interval is passed through untouched.
func TestOracleCheckDue_PassesDefaultIntervalWhenUnset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{name: "zero uses default", interval: 0, want: config.DefaultCheckInterval},
		{name: "negative uses default", interval: -time.Second, want: config.DefaultCheckInterval},
		{name: "positive kept", interval: 7 * time.Second, want: 7 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			oc := NewOracleChecker(adapter.NewEngine(http.DefaultClient), storage.NewOracleStore(), zap.NewNop())
			oc.AddConfig(OracleConfig{Network: "pocket", Interval: tt.interval})

			var gotNetwork string
			var gotInterval time.Duration
			calls := 0
			oc.CheckDue(context.Background(), time.Second, func(network string, interval time.Duration) bool {
				calls++
				gotNetwork, gotInterval = network, interval
				return false
			})
			if calls != 1 {
				t.Fatalf("due gate asked %d times, want 1", calls)
			}
			if gotNetwork != "pocket" {
				t.Errorf("due gate network = %q, want pocket", gotNetwork)
			}
			if gotInterval != tt.want {
				t.Errorf("due gate interval = %v, want %v", gotInterval, tt.want)
			}
		})
	}
}

// TestSchedulerStart_AnnouncesOnlyWhatItSchedules pins v2_scheduler.go Start:
// "Oracle checker scheduled" is logged only for an oracle checker that has
// networks, and "Archival checker scheduled" only when an archival store is
// wired AND a network configures archival.
func TestSchedulerStart_AnnouncesOnlyWhatItSchedules(t *testing.T) {
	t.Parallel()

	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(node.Close)

	tests := []struct {
		name         string
		archivalNet  bool // a network configures archival
		storeWired   bool // an ArchivalStore is passed to the scheduler
		oracleWired  bool // a non-nil OracleChecker is passed
		oracleNets   []string
		wantOracle   int
		wantArchival int
	}{
		{name: "nothing wired", wantOracle: 0, wantArchival: 0},
		{name: "oracle checker without networks", oracleWired: true, wantOracle: 0, wantArchival: 0},
		{name: "oracle checker with a network", oracleWired: true, oracleNets: []string{"pocket"}, wantOracle: 1},
		{name: "archival network but no store", archivalNet: true, wantArchival: 0},
		{name: "store but no archival network", storeWired: true, wantArchival: 0},
		{name: "store and archival network", archivalNet: true, storeWired: true, wantArchival: 1},
		{name: "everything wired", archivalNet: true, storeWired: true, oracleWired: true, oracleNets: []string{"pocket"}, wantOracle: 1, wantArchival: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var sched *MultiChainScheduler
			if tt.archivalNet {
				sched, _ = testMultiChainSetupWithArchival(t, node.URL)
			} else {
				sched, _, _ = testMultiChainSetup(t, node.URL)
			}
			sched.archivalStore = nil
			if tt.storeWired {
				sched.archivalStore = storage.NewArchivalStore()
			}
			sched.oracleChecker = nil
			if tt.oracleWired {
				oc := NewOracleChecker(adapter.NewEngine(http.DefaultClient), storage.NewOracleStore(), zap.NewNop())
				for _, n := range tt.oracleNets {
					oc.AddConfig(OracleConfig{Network: n, Interval: 30 * time.Second})
				}
				sched.oracleChecker = oc
			}
			core, logs := observer.New(zapcore.InfoLevel)
			sched.logger = zap.New(core)

			if err := sched.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			sched.Stop()
			drainTestPool(sched.pool)

			if n := logs.FilterMessage("Oracle checker scheduled").Len(); n != tt.wantOracle {
				t.Errorf("'Oracle checker scheduled' logged %d times, want %d", n, tt.wantOracle)
			}
			if n := logs.FilterMessage("Archival checker scheduled").Len(); n != tt.wantArchival {
				t.Errorf("'Archival checker scheduled' logged %d times, want %d", n, tt.wantArchival)
			}
		})
	}
}

// TestScheduleWebSocketHealthCheck_URLSelection pins v2_scheduler.go
// scheduleWebSocketHealthCheck: the "websocket" protocol is checked through the
// node's rpc endpoint (CometBFT serves it there), any other protocol through
// the URL it was given.
func TestScheduleWebSocketHealthCheck_URLSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		protocol    string
		wsLive      bool // the URL passed to the check serves WebSocket
		rpcLive     bool // node.Endpoints["rpc"] serves WebSocket
		wantHealthy bool
	}{
		{name: "websocket protocol uses rpc, rpc live", protocol: "websocket", wsLive: false, rpcLive: true, wantHealthy: true},
		{name: "websocket protocol uses rpc, rpc dead", protocol: "websocket", wsLive: true, rpcLive: false, wantHealthy: false},
		{name: "other protocol uses given URL, url live", protocol: "ws-alt", wsLive: true, rpcLive: false, wantHealthy: true},
		{name: "other protocol uses given URL, url dead", protocol: "ws-alt", wsLive: false, rpcLive: true, wantHealthy: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			live := cosmosNodeServer(t, true).URL
			dead := "http://" + deadAddr(t)
			pick := func(isLive bool) string {
				if isLive {
					return live
				}
				return dead
			}

			sched, healthStore := testHealthCheckScheduler(t)
			node := config.MultiChainNode{
				Name:      "test-node",
				Network:   "testnet",
				Endpoints: map[string]string{"rpc": pick(tt.rpcLive)},
			}
			sched.scheduleWebSocketHealthCheck(node, pick(tt.wsLive), tt.protocol, "rpc")
			drainTestPool(sched.pool)

			if got := healthStore.IsHealthy("testnet", "test-node", tt.protocol); got != tt.wantHealthy {
				t.Errorf("IsHealthy(%s) = %v, want %v (entry %+v)", tt.protocol, got, tt.wantHealthy, healthStore.GetHealth("testnet", "test-node", tt.protocol))
			}
		})
	}
}

// TestExternalWebSocketCheck_OnlyForRPCEndpoints pins external.go
// `endpointType == "rpc"` in validateEndpoint and RecoverFailedEndpoints: a
// freshly validated or recovered rpc endpoint gets its WebSocketAvailable flag
// from a real WebSocket probe, while any other type never has it set.
func TestExternalWebSocketCheck_OnlyForRPCEndpoints(t *testing.T) {
	t.Parallel()

	wsOf := func(t *testing.T, endpointStore *storage.ExternalEndpointStore, network, epType string) bool {
		t.Helper()
		eps := endpointStore.GetValidatedEndpoints(network, epType)
		if len(eps) != 1 {
			t.Fatalf("validated %s endpoints = %d, want 1", epType, len(eps))
		}
		return eps[0].WebSocketAvailable
	}

	t.Run("validation", func(t *testing.T) {
		t.Parallel()
		// One server that would answer a WebSocket probe, advertised as both
		// rpc and rest: only the rpc entry may be probed.
		wsSrv := cosmosNodeServer(t, true)
		ringSrv := httptest.NewServer(newStatusHandler(100, map[string]string{"rpc": wsSrv.URL, "rest": wsSrv.URL}))
		t.Cleanup(ringSrv.Close)

		checker, _, endpointStore := newTestExternalChecker(t, nil)
		ext := config.External{Name: "ws-validate", Rings: []string{ringSrv.URL}}
		if err := checker.queryRing(context.Background(), ext, ringSrv.URL, "pocket"); err != nil {
			t.Fatalf("queryRing: %v", err)
		}
		if !wsOf(t, endpointStore, "pocket", "rpc") {
			t.Error("rpc endpoint serving WebSocket: WebSocketAvailable = false, want true")
		}
		if wsOf(t, endpointStore, "pocket", "rest") {
			t.Error("rest endpoint: WebSocketAvailable = true, want false (only rpc is probed)")
		}
	})

	t.Run("rpc without websocket", func(t *testing.T) {
		t.Parallel()
		noWS := cosmosNodeServer(t, false)
		ringSrv := httptest.NewServer(newStatusHandler(100, map[string]string{"rpc": noWS.URL}))
		t.Cleanup(ringSrv.Close)

		checker, _, endpointStore := newTestExternalChecker(t, nil)
		ext := config.External{Name: "ws-validate-no", Rings: []string{ringSrv.URL}}
		if err := checker.queryRing(context.Background(), ext, ringSrv.URL, "pocket"); err != nil {
			t.Fatalf("queryRing: %v", err)
		}
		if wsOf(t, endpointStore, "pocket", "rpc") {
			t.Error("rpc endpoint without WebSocket: WebSocketAvailable = true, want false")
		}
	})

	t.Run("recovery", func(t *testing.T) {
		t.Parallel()
		wsSrv := cosmosNodeServer(t, true)
		checker, _, endpointStore := newTestExternalChecker(t, nil)
		seedFailedEndpoint(t, endpointStore, "ws-recover", "http://ring.example.com", "pocket", "rpc", wsSrv.URL, 100)
		seedFailedEndpoint(t, endpointStore, "ws-recover", "http://ring.example.com", "pocket", "rest", wsSrv.URL, 100)
		if n := len(endpointStore.GetFailedEndpoints()); n != 2 {
			t.Fatalf("failed endpoints before recovery = %d, want 2", n)
		}

		checker.RecoverFailedEndpoints(context.Background())

		if !wsOf(t, endpointStore, "pocket", "rpc") {
			t.Error("recovered rpc endpoint serving WebSocket: WebSocketAvailable = false, want true")
		}
		if wsOf(t, endpointStore, "pocket", "rest") {
			t.Error("recovered rest endpoint: WebSocketAvailable = true, want false (only rpc is probed)")
		}
	})
}

// TestCheckExternalRings_LogsOnlyFailedChecks pins v2_scheduler.go
// checkExternalRings `err != nil`: a failing CheckExternal (an external with no
// rings) is logged at Debug with its external and network; a healthy one is not.
func TestCheckExternalRings_LogsOnlyFailedChecks(t *testing.T) {
	t.Parallel()

	ring := httptest.NewServer(newStatusHandler(12345, map[string]string{}))
	t.Cleanup(ring.Close)

	loader := testutil.NewMultiChainLoader(t, `
listen: ":3000"
timeouts:
  health_check: 2s
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: rest
        listen: ":8080"
externals:
  - name: good
    rings:
      - "`+ring.URL+`"
internals:
  - name: node-1
    network: pocket
    endpoints:
      rest: "http://localhost:1317"
`)
	// The validator refuses an external without rings, so add one after load
	// (nothing else reads the config yet).
	cfg := loader.Get()
	cfg.Externals = append(cfg.Externals, config.External{Name: "no-rings"})

	registry := adapter.NewRegistry()
	core, logs := observer.New(zapcore.DebugLevel)
	sched := NewMultiChainScheduler(adapter.NewEngine(http.DefaultClient), registry, storage.NewHeightStore(), storage.NewHealthStore(), nil,
		storage.NewCache("", zap.NewNop()), storage.NewExternalEndpointStore(zap.NewNop()), nil, loader, newTestPool(), zap.New(core))

	sched.checkExternalRings()
	drainTestPool(sched.pool)

	failed := logs.FilterMessage("External check failed").All()
	if len(failed) != 1 {
		t.Fatalf("'External check failed' logged %d times, want 1 (entries: %v)", len(failed), failed)
	}
	fields := failed[0].ContextMap()
	if fields["external"] != "no-rings" {
		t.Errorf("failed check external = %v, want no-rings", fields["external"])
	}
	if fields["network"] != "pocket" {
		t.Errorf("failed check network = %v, want pocket", fields["network"])
	}
}

// TestCheckArchivalStatus_ZeroHeightIsNotArchival pins v2_scheduler.go
// `height > 0` in checkArchivalNetwork: a node that answers the archival query
// without error but with height 0 does not hold the block, so it is not
// archival.
func TestCheckArchivalStatus_ZeroHeightIsNotArchival(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sdk_block":{"header":{"height":"0"}}}`))
	}))
	t.Cleanup(srv.Close)

	sched, archivalStore := testMultiChainSetupWithArchival(t, srv.URL)
	sched.checkArchivalStatus()
	drainTestPool(sched.pool)

	status := archivalStore.GetStatus("pocket", "seed-one")
	if status == nil {
		t.Fatal("no archival status recorded for seed-one")
	}
	if status.IsArchival {
		t.Error("seed-one marked archival for a block answered with height 0")
	}
}

// TestGoOnce_ReleasesTheCheckWhenThePoolRefuses pins v2_scheduler.go goOnce
// `err != nil` on pool.Go: when the pool refuses the task (stopped), the
// check's in-flight flag is released so the check can run once a pool accepts
// it again; while a task is running, a second submission of the same key is
// dropped.
func TestGoOnce_ReleasesTheCheckWhenThePoolRefuses(t *testing.T) {
	t.Parallel()

	t.Run("stopped pool", func(t *testing.T) {
		t.Parallel()
		s := &MultiChainScheduler{pool: newTestPool()}
		drainTestPool(s.pool) // stopped: pool.Go now returns an error

		var ran atomic.Int32
		s.goOnce("k", func() { ran.Add(1) })
		if n := ran.Load(); n != 0 {
			t.Fatalf("task ran %d times on a stopped pool, want 0", n)
		}

		s.pool = newTestPool()
		s.goOnce("k", func() { ran.Add(1) })
		drainTestPool(s.pool)
		if n := ran.Load(); n != 1 {
			t.Errorf("task ran %d times after the pool recovered, want 1 (flag left set by the refused submission)", n)
		}
	})

	t.Run("running task blocks a second submission", func(t *testing.T) {
		t.Parallel()
		s := &MultiChainScheduler{pool: newTestPool()}
		started := make(chan struct{})
		release := make(chan struct{})
		var ran atomic.Int32
		s.goOnce("k", func() {
			ran.Add(1)
			close(started)
			<-release
		})
		<-started
		s.goOnce("k", func() { ran.Add(1) })
		s.goOnce("other", func() { ran.Add(10) }) // other keys are independent
		close(release)
		drainTestPool(s.pool)
		if n := ran.Load(); n != 11 {
			t.Errorf("ran sum = %d, want 11 (k once, other once)", n)
		}
	})
}

// TestSchedulerTick_OracleChecksFollowTheirInterval pins v2_scheduler.go tick:
// the oracle task is gated per network by the oracle's own interval through
// due("oracle:"+network, ...), so an oracle with a 10s interval is queried at
// t0 and t0+10s but not at t0+1s, whatever the height check does.
func TestSchedulerTick_OracleChecksFollowTheirInterval(t *testing.T) {
	t.Parallel()

	var node countingNode
	nodeSrv := node.server(t)
	var oracleHits atomic.Int32
	oracleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		oracleHits.Add(1)
		_, _ = w.Write([]byte(`{"result":"0x64"}`))
	}))
	t.Cleanup(oracleSrv.Close)

	sched, _, _ := testMultiChainSetup(t, nodeSrv.URL)
	oracleStore := storage.NewOracleStore()
	oc := NewOracleChecker(adapter.NewEngine(&http.Client{Timeout: 2 * time.Second}), oracleStore, zap.NewNop())
	check := adapter.CheckConfig{Method: "POST", URLPath: "/", ResponsePath: ".result", Protocol: "jsonrpc"}
	oc.AddConfig(OracleConfig{Network: "pocket", Interval: 10 * time.Second, Oracles: []OracleEndpoint{{URL: oracleSrv.URL, Check: check}}})
	sched.oracleChecker = oc

	t0 := time.Now()
	steps := []struct {
		at       time.Duration
		wantHits int32
	}{
		{at: 0, wantHits: 1},
		{at: time.Second, wantHits: 1},
		{at: 9 * time.Second, wantHits: 1},
		{at: 10 * time.Second, wantHits: 2},
	}
	for _, st := range steps {
		sched.pool = newTestPool() // drainTestPool stops the pool
		sched.tick(t0.Add(st.at))
		drainTestPool(sched.pool)
		if got := oracleHits.Load(); got != st.wantHits {
			t.Fatalf("after tick at t0+%v oracle queried %d times, want %d", st.at, got, st.wantHits)
		}
	}
	if h, ok := oracleStore.Get("pocket"); !ok || h != 100 {
		t.Errorf("oracle height = %d, %v; want 100, true", h, ok)
	}
}

// TestExternalCheckerClose_WarnsOnlyForConnectionsThatFailToClose pins
// external.go Close `err != nil`: closing a connection that is already closed
// is logged as a warning naming its URL; a connection that closes cleanly is
// not, and the cache is emptied either way.
func TestExternalCheckerClose_WarnsOnlyForConnectionsThatFailToClose(t *testing.T) {
	t.Parallel()

	checker, _, _ := newTestExternalChecker(t, nil)
	core, logs := observer.New(zapcore.WarnLevel)
	checker.logger = zap.New(core)

	open, err := grpcutil.NewConnection("127.0.0.1:1", true, keepalive.ClientParameters{})
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	already, err := grpcutil.NewConnection("127.0.0.1:2", true, keepalive.ClientParameters{})
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	if err := already.Close(); err != nil {
		t.Fatalf("pre-closing a connection: %v", err)
	}
	checker.grpcConnections.Store("open", open)
	checker.grpcConnections.Store("already-closed", already)

	checker.Close()

	warns := logs.FilterMessage("Failed to close external gRPC connection").All()
	if len(warns) != 1 {
		t.Fatalf("close warnings = %d, want 1 (entries: %v)", len(warns), warns)
	}
	if url := warns[0].ContextMap()["url"]; url != "already-closed" {
		t.Errorf("warning url = %v, want already-closed", url)
	}
	if n := checker.grpcConnections.Size(); n != 0 {
		t.Errorf("cached connections after Close = %d, want 0", n)
	}
}

// firstBytesOfDial runs CheckWebSocket against a listener that records the
// first 16 bytes the client sends, and returns them.
func firstBytesOfDial(t *testing.T, target func(addr string) string) []byte {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	got := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- nil
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 16)
		if _, err := io.ReadFull(conn, buf); err != nil {
			got <- nil
			return
		}
		got <- buf
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if CheckWebSocket(ctx, target(ln.Addr().String()), zap.NewNop()) {
		t.Fatal("CheckWebSocket succeeded against a listener that never answers")
	}
	_ = ln.Close() // unblocks Accept when the client never connected
	return <-got
}

// TestCheckWebSocket_URLConversion pins websocket.go URL handling: a trailing
// slash is trimmed (no "//websocket"), http/ws schemes dial in clear text
// ("GET /websocket"), and https, wss and a bare host dial TLS (first byte 0x16,
// a TLS handshake record).
func TestCheckWebSocket_URLConversion(t *testing.T) {
	t.Parallel()

	const clearText = "GET /websocket H"
	tests := []struct {
		name   string
		target func(addr string) string
		want   string // exact first 16 bytes; empty means "a TLS handshake record"
	}{
		{name: "http", target: func(a string) string { return "http://" + a }, want: clearText},
		{name: "http trailing slash", target: func(a string) string { return "http://" + a + "/" }, want: clearText},
		{name: "ws", target: func(a string) string { return "ws://" + a }, want: clearText},
		{name: "ws trailing slash", target: func(a string) string { return "ws://" + a + "/" }, want: clearText},
		{name: "https", target: func(a string) string { return "https://" + a }},
		{name: "https trailing slash", target: func(a string) string { return "https://" + a + "/" }},
		{name: "wss", target: func(a string) string { return "wss://" + a }},
		{name: "bare host", target: func(a string) string { return a }},
		{name: "bare host trailing slash", target: func(a string) string { return a + "/" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			first := firstBytesOfDial(t, tt.target)
			if len(first) != 16 {
				t.Fatalf("listener saw %d bytes, want 16", len(first))
			}
			if tt.want != "" {
				if string(first) != tt.want {
					t.Errorf("request starts %q, want %q", first, tt.want)
				}
				return
			}
			if first[0] != 0x16 {
				t.Errorf("first byte = %#x, want 0x16 (TLS handshake)", first[0])
			}
		})
	}
}

// TestCheckWebSocket_SlashOnlyURLDoesNotPanic pins websocket.go: a URL that is
// only "/" is empty after the slash is trimmed; it must be rejected (malformed
// URL), not indexed.
func TestCheckWebSocket_SlashOnlyURLDoesNotPanic(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if CheckWebSocket(ctx, "/", zap.NewNop()) {
		t.Error("CheckWebSocket(\"/\") = true, want false")
	}
}

// TestCheckWebSocket_TrailingSlashReachesNode: end to end, a node URL with a
// trailing slash still reaches /websocket on a real WebSocket endpoint.
func TestCheckWebSocket_TrailingSlashReachesNode(t *testing.T) {
	t.Parallel()
	srv := cosmosNodeServer(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !CheckWebSocket(ctx, srv.URL+"/", zap.NewNop()) {
		t.Error("CheckWebSocket with a trailing slash = false, want true")
	}
}

// TestCheckWebSocket_HandshakeIsBoundedByThreeSeconds pins websocket.go
// `HandshakeTimeout: 3 * time.Second`: a server that accepts the connection but
// never answers the upgrade is given up on after the handshake timeout, long
// before a generous caller context would expire. (Takes about 3s: the bound
// itself is what is measured.)
func TestCheckWebSocket_HandshakeIsBoundedByThreeSeconds(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		_ = ln.Close()
	})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		<-done // hold the connection open, never answer
		_ = conn.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	ok := CheckWebSocket(ctx, "http://"+ln.Addr().String(), zap.NewNop())
	elapsed := time.Since(start)
	if ok {
		t.Fatal("CheckWebSocket succeeded against a server that never answers")
	}
	if elapsed < 2*time.Second || elapsed > 5*time.Second {
		t.Errorf("gave up after %v, want about 3s (the handshake timeout, not the 8s context)", elapsed)
	}
}
