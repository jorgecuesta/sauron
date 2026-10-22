package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sauron/internal/fakenode"
)

// pipelineNode is one in-process fake Cosmos node behind the pipeline.
type pipelineNode struct {
	node                 *fakenode.Node
	rest, rpc, grpc, adm string
	stop                 context.CancelFunc
	done                 chan error
}

// pipeline is a Sauron server built and started exactly as in production
// (New, then Start: scheduler, stores, selector, proxies, status API), in
// front of two fake nodes named a and b. Health checks run every second.
type pipeline struct {
	s                         *Server
	a, b                      string // node names, unique per pipeline
	nodes                     map[string]*pipelineNode
	status, rest, rpc, grpcAd string
}

// nextPort hands out ports below Linux's ephemeral range (32768-60999), so a
// port picked here is never taken meanwhile by another test's ":0" listener.
// The base varies with the PID to keep parallel test binaries apart.
var nextPort atomic.Int32

func init() { nextPort.Store(int32(20000 + (os.Getpid()%100)*100)) }

// freeAddr returns a 127.0.0.1 address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	for i := 0; i < 1000; i++ {
		addr := "127.0.0.1:" + strconv.Itoa(int(nextPort.Add(1)))
		if l, err := net.Listen("tcp", addr); err == nil {
			_ = l.Close()
			return addr
		}
	}
	t.Fatal("no free port")
	return ""
}

// pipelineSeq makes node names unique per pipeline: metrics are process-wide,
// so parallel pipelines must not share label values.
var pipelineSeq atomic.Int32

func startFakeNode(t *testing.T, name string) *pipelineNode {
	t.Helper()
	var ls fakenode.Listeners
	var addrs [4]string
	// Ports from freeAddr, not ":0": once this node is killed, the kernel
	// could hand its ephemeral port to another test's node, and Sauron would
	// reach that node at this one's address.
	for i, dst := range []*net.Listener{&ls.REST, &ls.RPC, &ls.GRPC, &ls.Admin} {
		addr := freeAddr(t)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("listen %s: %v", addr, err)
		}
		*dst, addrs[i] = l, addr
	}
	// A block interval of an hour keeps every node at the same height, so
	// selection is round-robin over the healthy ones.
	n := fakenode.NewNode(name, 1000, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	pn := &pipelineNode{node: n, rest: addrs[0], rpc: addrs[1], grpc: addrs[2], adm: addrs[3], stop: cancel, done: make(chan error, 1)}
	go func() { pn.done <- fakenode.Run(ctx, n, ls) }()
	t.Cleanup(pn.kill)
	return pn
}

// kill stops the node's servers, so its addresses refuse connections.
func (pn *pipelineNode) kill() {
	pn.stop()
	select {
	case <-pn.done:
	case <-time.After(5 * time.Second):
	}
}

const pipelineYAML = `
listen: "%s"
timeouts:
  health_check: 1s
  proxy: 5s
networks:
  - name: pocket
    type: cosmos
    height_check:
      protocol: rpc
      interval: 1s
    endpoints:
      - protocol: rest
        listen: "%s"
      - protocol: rpc
        listen: "%s"
      - protocol: grpc
        listen: "%s"
    circuit_breaker:
      threshold: 3
      http_codes: [502, 503]
      retry_attempts: 1
      retry_backoff: 10ms
internals:
%s`

// startPipeline starts two fake nodes and a Sauron server in front of them,
// and waits until both are routable on every protocol.
func startPipeline(t *testing.T) *pipeline {
	t.Helper()
	seq := pipelineSeq.Add(1)
	names := []string{fmt.Sprintf("p%d-a", seq), fmt.Sprintf("p%d-b", seq)}
	p := &pipeline{a: names[0], b: names[1], nodes: map[string]*pipelineNode{}}
	var internals strings.Builder
	for _, name := range names {
		pn := startFakeNode(t, name)
		p.nodes[name] = pn
		fmt.Fprintf(&internals, `  - name: %s
    network: pocket
    grpc_insecure: true
    endpoints:
      rest: "http://%s"
      rpc: "http://%s"
      grpc: "%s"
`, name, pn.rest, pn.rpc, pn.grpc)
	}
	p.status, p.rest, p.rpc, p.grpcAd = freeAddr(t), freeAddr(t), freeAddr(t), freeAddr(t)
	path := t.TempDir() + "/config.yaml"
	cfg := fmt.Sprintf(pipelineYAML, p.status, p.rest, p.rpc, p.grpcAd, internals.String())
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	p.s = s
	t.Cleanup(s.Shutdown)

	// Start's listeners run in goroutines and report failures on errCh: wait
	// until every one accepts connections.
	deadline := time.Now().Add(5 * time.Second)
	for _, addr := range []string{p.status, p.rest, p.rpc, p.grpcAd} {
		for {
			select {
			case err := <-s.errCh:
				t.Fatalf("server failed to start: %v", err)
			default:
			}
			if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
				_ = c.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s not listening 5s after Start", addr)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	for _, name := range names {
		p.waitRoutable(t, name, true)
	}
	return p
}

// routable reports whether the scheduler has found name healthy on every
// protocol with WebSocket support, i.e. the selector can pick it for anything.
func (p *pipeline) routable(name string) bool {
	for _, proto := range []string{"rest", "rpc", "grpc"} {
		if !p.s.healthStore.IsHealthy("pocket", name, proto) {
			return false
		}
	}
	m, ok := p.s.store.GetByNetwork("pocket", "rpc")[name]
	return ok && m.Height > 0 && m.WebSocketAvailable
}

// excluded reports whether the scheduler has found name unhealthy on every
// protocol, so the selector picks it for nothing. (Each protocol is checked on
// its own, so one can still be healthy for a moment after another failed.)
func (p *pipeline) excluded(name string) bool {
	for _, proto := range []string{"rest", "rpc", "grpc"} {
		if p.s.healthStore.IsHealthy("pocket", name, proto) {
			return false
		}
	}
	return true
}

// waitRoutable waits until name is routable on every protocol (want true) or
// excluded from every protocol (want false), as the health checks see it.
func (p *pipeline) waitRoutable(t *testing.T, name string, want bool) {
	t.Helper()
	done := func() bool { return p.routable(name) }
	if !want {
		done = func() bool { return p.excluded(name) }
	}
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("node %s not %s after 10s", name, map[bool]string{true: "routable", false: "excluded"}[want])
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// httpGet does a GET and returns status, X-Fake-Node and body.
func httpGet(t *testing.T, url string) (int, string, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("X-Fake-Node"), string(body)
}

// httpPost does a JSON POST and returns status, X-Fake-Node and body.
func httpPost(t *testing.T, url, body string) (int, string, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("X-Fake-Node"), string(b)
}

// metricValue reads one sample from the status API's /metrics: the sum of the
// series of name whose labels contain every pair in labels.
func (p *pipeline) metricValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	_, _, body := httpGet(t, "http://"+p.status+"/metrics")
	sum := 0.0
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+"{") {
			continue
		}
		match := true
		for k, v := range labels {
			if !strings.Contains(line, k+`="`+v+`"`) {
				match = false
			}
		}
		if !match {
			continue
		}
		f := strings.Fields(line)
		v, err := strconv.ParseFloat(f[len(f)-1], 64)
		if err == nil {
			sum += v
		}
	}
	return sum
}
