package server

import (
	"context"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	"sauron/config"
	"sauron/metrics"
	"sauron/proxy"
)

// startEchoBackend starts a gRPC server answering every method with an empty
// message and returns its address.
func startEchoBackend(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ interface{}, stream grpc.ServerStream) error {
		var req emptypb.Empty
		if err := stream.RecvMsg(&req); err != nil {
			return err
		}
		return stream.SendMsg(&emptypb.Empty{})
	}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

const reconcileYAML = `
listen: ":3000"
timeouts:
  health_check: 1s
  proxy: 10s
networks:
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: rpc
        listen: ":18181"
      - protocol: grpc
        listen: ":18182"
internals:
  - name: node-a
    network: pocket
    grpc_insecure: true
    endpoints:
      rpc: "http://127.0.0.1:18199"
      grpc: "NODE_A_GRPC"
NODE_B
externals:
  - name: partner
    rings: ["https://ring-partner.example.com"]
EXTERNAL_GONE
`

const reconcileNodeB = `  - name: node-b
    network: pocket
    grpc_insecure: true
    endpoints:
      rpc: "http://127.0.0.1:18198"
      grpc: "NODE_B_GRPC"`

const reconcileExternalGone = `  - name: gone
    rings: ["https://ring-gone.example.com"]`

func reconcileConfig(nodeA, nodeB string, withB, withGone bool) string {
	y := strings.Replace(reconcileYAML, "NODE_A_GRPC", nodeA, 1)
	b, g := "", ""
	if withB {
		b = strings.Replace(reconcileNodeB, "NODE_B_GRPC", nodeB, 1)
	}
	if withGone {
		g = reconcileExternalGone
	}
	y = strings.Replace(y, "NODE_B", b, 1)
	return strings.Replace(y, "EXTERNAL_GONE", g, 1)
}

// nodeHeightSeries counts the sauron_node_height series of network/node.
func nodeHeightSeries(t *testing.T, network, node string) int {
	t.Helper()
	ch := make(chan prometheus.Metric, 256)
	metrics.NodeHeight.Collect(ch)
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
		if labels["network"] == network && labels["node"] == node {
			n++
		}
	}
	return n
}

// TestServer_ReloadForgetsRemovedNodesAndExternals (#20): with production
// wiring (New, then the reload hook Start installs), removing a node or an
// external, or changing a node's gRPC URL, drops their state, metrics and
// pooled connections; what stays configured is untouched and keeps serving.
func TestServer_ReloadForgetsRemovedNodesAndExternals(t *testing.T) {
	addrA, addrB, addrC := startEchoBackend(t), startEchoBackend(t), startEchoBackend(t)
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte(reconcileConfig(addrA, addrB, true, true)), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The gRPC proxy as startGRPCProxy builds it, served on a test listener.
	p := proxy.NewGRPCProxy(s.selector, s.configLoader, s.endpointStore, s.logger, "pocket", s.circuitBreaker)
	s.grpcProxies = append(s.grpcProxies, p)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	go func() { _ = p.GetServer().Serve(lis) }()
	t.Cleanup(func() { p.GetServer().Stop(); _ = p.Close() })
	s.watchConfig()

	// State the checkers and proxies would have built for both nodes.
	for _, node := range []string{"node-a", "node-b"} {
		s.store.Update("pocket", node, "rpc", 100, time.Millisecond, "internal")
		s.healthStore.SetHealthy("pocket", node, "grpc")
		metrics.NodeHeight.WithLabelValues("pocket", node, "rpc", "internal").Set(100)
	}
	s.circuitBreaker.RecordError("pocket", "node-b", "grpc", 100)
	s.archivalStore.SetArchival("pocket", "node-b")
	s.endpointStore.StoreAdvertised("partner", "https://ring-partner.example.com", "pocket", "rest", "https://partner-api.example.com")
	s.endpointStore.StoreAdvertised("gone", "https://ring-gone.example.com", "pocket", "rest", "https://gone-api.example.com")

	conn, err := grpc.NewClient("passthrough:///"+lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	call := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return conn.Invoke(ctx, "/test.Service/Query", &emptypb.Empty{}, &emptypb.Empty{})
	}
	for i := 0; i < 2; i++ {
		if err := call(); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if pooled := p.PooledAddresses(); len(pooled) != 2 {
		t.Fatalf("pooled = %v, want node-a and node-b", pooled)
	}

	// Reload without node-b and the "gone" external.
	if err := os.WriteFile(path, []byte(reconcileConfig(addrA, addrB, false, false)), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	if err := s.configLoader.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if _, ok := s.store.GetByNetwork("pocket", "rpc")["node-b"]; ok {
		t.Error("removed node-b still has a height")
	}
	if _, ok := s.store.GetByNetwork("pocket", "rpc")["node-a"]; !ok {
		t.Error("kept node-a lost its height")
	}
	if s.healthStore.GetHealth("pocket", "node-b", "grpc") != nil {
		t.Error("removed node-b still has health")
	}
	if !s.healthStore.IsHealthy("pocket", "node-a", "grpc") {
		t.Error("kept node-a lost its health")
	}
	if got := s.circuitBreaker.GetErrorCount("pocket", "node-b", "grpc"); got != 0 {
		t.Errorf("removed node-b breaker count = %d, want 0", got)
	}
	if s.archivalStore.GetStatus("pocket", "node-b") != nil {
		t.Error("removed node-b still has archival status")
	}
	if n := nodeHeightSeries(t, "pocket", "node-b"); n != 0 {
		t.Errorf("sauron_node_height still exports node-b (%d series)", n)
	}
	if n := nodeHeightSeries(t, "pocket", "node-a"); n != 1 {
		t.Errorf("sauron_node_height lost node-a (%d series)", n)
	}
	if urls := s.endpointStore.URLs("rest"); urls["https://gone-api.example.com"] || !urls["https://partner-api.example.com"] {
		t.Errorf("external endpoints after reload = %v, want partner only", urls)
	}
	if got := p.PooledAddresses(); len(got) != 1 || got[0] != addrA {
		t.Errorf("pooled after removing node-b = %v, want only %s", got, addrA)
	}

	// node-a moves to another gRPC URL: the old connection goes.
	if err := os.WriteFile(path, []byte(reconcileConfig(addrC, addrB, false, false)), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	if err := s.configLoader.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := p.PooledAddresses(); len(got) != 0 {
		t.Errorf("pooled after node-a's URL changed = %v, want none (old address dropped)", got)
	}
	if err := call(); err != nil {
		t.Errorf("call after the URL change: %v", err)
	}
	if got := p.PooledAddresses(); len(got) != 1 || got[0] != addrC {
		t.Errorf("pooled after a call = %v, want only the new %s", got, addrC)
	}
}

// A check that was already running when the reload landed can write a removed
// node back; the delayed second pass drops it again.
func TestServer_ReconcileSecondPassCatchesLateWrites(t *testing.T) {
	addrA, addrB := startEchoBackend(t), startEchoBackend(t)
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte(reconcileConfig(addrA, addrB, true, false)), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.watchConfig()

	if err := os.WriteFile(path, []byte(reconcileConfig(addrA, addrB, false, false)), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	var reloads atomic.Int64
	s.configLoader.OnReload(func(*config.MultiChainConfig) { reloads.Add(1) })
	if err := s.configLoader.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	// The file watcher reloads too; wait until it is quiet, so only the
	// delayed second pass can remove what is written next.
	for last, quietSince := reloads.Load(), time.Now(); time.Since(quietSince) < 500*time.Millisecond; time.Sleep(20 * time.Millisecond) {
		if n := reloads.Load(); n != last {
			last, quietSince = n, time.Now()
		}
	}
	// A late check result for the removed node.
	s.store.Update("pocket", "node-b", "rpc", 100, time.Millisecond, "internal")
	time.Sleep(100 * time.Millisecond)
	if _, ok := s.store.GetByNetwork("pocket", "rpc")["node-b"]; !ok {
		t.Fatal("late write removed by something other than the second pass (precondition)")
	}

	deadline := time.Now().Add(4 * time.Second) // health_check 1s + 1s grace
	for {
		if _, ok := s.store.GetByNetwork("pocket", "rpc")["node-b"]; !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("late write for the removed node survived the second pass")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The delayed second pass does not run once the server has shut down, and
// Shutdown can be called twice.
func TestServer_ReconcileSecondPassSkippedAfterShutdown(t *testing.T) {
	addrA, addrB := startEchoBackend(t), startEchoBackend(t)
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte(reconcileConfig(addrA, addrB, true, false)), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.watchConfig()
	if err := os.WriteFile(path, []byte(reconcileConfig(addrA, addrB, false, false)), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	if err := s.configLoader.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	s.Shutdown()
	s.Shutdown() // idempotent

	// Written after shutdown: no second pass may remove it.
	time.Sleep(300 * time.Millisecond) // let any watcher reload settle
	s.store.Update("pocket", "node-b", "rpc", 100, time.Millisecond, "internal")
	time.Sleep(2500 * time.Millisecond) // past health_check 1s + 1s grace
	if _, ok := s.store.GetByNetwork("pocket", "rpc")["node-b"]; !ok {
		t.Error("the second reconciliation pass ran after Shutdown")
	}
}
