package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sauron/internal/grpcutil"
	"sauron/internal/testutil"
	"sauron/metrics"
	"sauron/selector"
	"sauron/storage"

	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// Tests in this file pin behaviour that the mutation run of the proxy package
// (issue #19) showed to be unobserved. Every network, node and service name
// here is unique to its test, so the global Prometheus counters read below
// are not moved by any other test.

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// counterValue reads a Prometheus counter.
func counterValue(t *testing.T, c interface{ Write(*dto.Metric) error }) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// closedTCPAddr returns a local address nothing listens on. Port 1 is
// privileged and never bound by tests, so a dial is always refused; a port
// bound and closed here could be reused meanwhile by a parallel test's server.
func closedTCPAddr(t *testing.T) string {
	t.Helper()
	return "127.0.0.1:1"
}

// debugLogger returns a logger recording everything, and its recorded entries.
func debugLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

// ---------------------------------------------------------------------------
// HTTPProxy
// ---------------------------------------------------------------------------

// The transport must carry the configured proxy timeout as its response-header
// timeout, and the fixed idle and TLS-handshake timeouts.
func TestMutationKill_HTTPProxy_TransportTimeouts(t *testing.T) {
	t.Parallel()

	p, _, _ := newCBHTTPProxy(t, "mut-transport-net", "http://127.0.0.1:1")

	if got, want := p.transport.ResponseHeaderTimeout, 5*time.Second; got != want {
		t.Errorf("ResponseHeaderTimeout = %v, want %v (timeouts.proxy)", got, want)
	}
	if got, want := p.transport.IdleConnTimeout, 90*time.Second; got != want {
		t.Errorf("IdleConnTimeout = %v, want %v", got, want)
	}
	if got, want := p.transport.TLSHandshakeTimeout, 10*time.Second; got != want {
		t.Errorf("TLSHandshakeTimeout = %v, want %v", got, want)
	}
}

// A node that keeps answering a retriable code is tried exactly 1 +
// retry_attempts times, then the client gets 502. Each failure counts against
// the breaker and in ProxyErrors under its status code, and the attempt
// numbers in the logs are 1-based.
func TestMutationKill_HTTPProxy_RetryBudgetExhausted(t *testing.T) {
	t.Parallel()

	const network = "mut-budget-net"
	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A runaway retry loop eventually gets a 200 instead of spinning forever.
		if hits.Add(1) > 8 {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer backend.Close()

	p, cb, _ := newCBHTTPProxy(t, network, backend.URL) // retry_attempts: 2
	logger, logs := debugLogger()
	p.logger = logger
	retriable := metrics.ProxyErrors.WithLabelValues(network, "node-1", "rest", "503", "retriable")
	retriableBefore := counterValue(t, retriable) // global: moved by earlier runs with -count

	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/status", nil))

	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 once the retries are spent", rr.Code)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("backend hits = %d, want 3 (1 try + 2 retries)", got)
	}
	if got := cb.GetErrorCount(network, "node-1", "rest"); got != 3 {
		t.Errorf("circuit breaker errors = %d, want 3 (one per failed attempt)", got)
	}
	if got := counterValue(t, retriable) - retriableBefore; got != 3 {
		t.Errorf("ProxyErrors{503,retriable} moved by %v, want 3", got)
	}

	var routed []string
	for _, e := range logs.FilterMessage("Routing decision made").All() {
		routed = append(routed, e.ContextMap()["attempt"].(string))
	}
	if got, want := strings.Join(routed, ","), "1/3,2/3,3/3"; got != want {
		t.Errorf("routing attempts logged = %q, want %q", got, want)
	}
	var failed []int64
	for _, e := range logs.FilterMessage("Retriable error on attempt").All() {
		failed = append(failed, e.ContextMap()["attempt"].(int64))
	}
	if len(failed) != 3 || failed[0] != 1 || failed[1] != 2 || failed[2] != 3 {
		t.Errorf("failed attempts logged = %v, want [1 2 3]", failed)
	}
}

// A connection failure (no status code) is retried and counts against the
// breaker, but is not recorded as a status code in ProxyErrors.
func TestMutationKill_HTTPProxy_ConnectionFailureHasNoStatusMetric(t *testing.T) {
	t.Parallel()

	const network = "mut-connfail-net"
	backendURL := "http://" + closedTCPAddr(t) // refuses connections

	p, cb, _ := newCBHTTPProxy(t, network, backendURL)
	zero := metrics.ProxyErrors.WithLabelValues(network, "node-1", "rest", "0", "retriable")
	zeroBefore := counterValue(t, zero)

	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/status", nil))

	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rr.Code)
	}
	if got := cb.GetErrorCount(network, "node-1", "rest"); got != 3 {
		t.Errorf("circuit breaker errors = %d, want 3 (connection failures count)", got)
	}
	if got := counterValue(t, zero) - zeroBefore; got != 0 {
		t.Errorf("ProxyErrors{status 0,retriable} moved by %v, want 0 (no response, no status)", got)
	}
}

// A served request resets the node's consecutive-error count, also when it
// follows failed attempts of the same request.
func TestMutationKill_HTTPProxy_SuccessResetsBreakerCount(t *testing.T) {
	t.Parallel()

	const network = "mut-reset-net"
	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, cb, _ := newCBHTTPProxy(t, network, backend.URL)
	cb.RecordError(network, "node-1", "rest", 100)
	cb.RecordError(network, "node-1", "rest", 100)

	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/status", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (second attempt succeeds)", rr.Code)
	}
	if got := cb.GetErrorCount(network, "node-1", "rest"); got != 0 {
		t.Errorf("circuit breaker errors after a served request = %d, want 0", got)
	}
}

// retry_backoff is waited between attempts and only between attempts: the
// first attempt goes out at once, the retry one backoff after the failure.
func TestMutationKill_HTTPProxy_BackoffOnlyBetweenAttempts(t *testing.T) {
	t.Parallel()

	const (
		network = "mut-backoff-net"
		backoff = 500 * time.Millisecond
	)
	var (
		mu   sync.Mutex
		hits []time.Time
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits = append(hits, time.Now())
		n := len(hits)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, _, _ := newCBHTTPProxy(t, network, backend.URL)
	// The loaded config is not shared with another test: this network is ours.
	p.getCircuitBreakerConfig().RetryBackoff = backoff

	start := time.Now()
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/status", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 2 {
		t.Fatalf("backend hits = %d, want 2", len(hits))
	}
	if first := hits[0].Sub(start); first >= backoff {
		t.Errorf("first attempt reached the backend after %v, want no backoff before it", first)
	}
	if gap := hits[1].Sub(hits[0]); gap < backoff {
		t.Errorf("retry came %v after the failure, want at least the %v backoff", gap, backoff)
	}
}

// The target's path and query and the request's are joined as
// httputil.NewSingleHostReverseProxy does, keeping percent-encoding.
func TestMutationKill_HTTPProxy_OutgoingURLJoin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		targetSuffix string // appended to the backend URL in the node's config
		requestURI   string
		wantURI      string
	}{
		{name: "request query only", targetSuffix: "", requestURI: "/p?y=2", wantURI: "/p?y=2"},
		{name: "target query only", targetSuffix: "?x=1", requestURI: "/p", wantURI: "/p?x=1"},
		{name: "both queries are merged", targetSuffix: "?x=1", requestURI: "/p?y=2", wantURI: "/p?x=1&y=2"},
		{name: "encoded slash in target and request path", targetSuffix: "/base%2Fx", requestURI: "/a%2Fb", wantURI: "/base%2Fx/a%2Fb"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var gotURI atomic.Value
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotURI.Store(r.RequestURI)
				w.WriteHeader(http.StatusOK)
			}))
			defer backend.Close()

			p, _, _ := newCBHTTPProxy(t, "mut-join-net-"+strconv.Itoa(i), backend.URL+tc.targetSuffix)
			rr := httptest.NewRecorder()
			p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.requestURI, nil))

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			if got, _ := gotURI.Load().(string); got != tc.wantURI {
				t.Errorf("backend saw %q, want %q", got, tc.wantURI)
			}
		})
	}
}

// Status codes from 400 on are counted as http_error; only 5xx also count
// against an external endpoint. The boundary codes decide which.
func TestMutationKill_HTTPProxy_ErrorStatusAccounting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code        int
		wantHTTPErr float64
		wantTracked int
	}{
		{code: 200, wantHTTPErr: 0, wantTracked: 0},
		{code: 399, wantHTTPErr: 0, wantTracked: 0},
		{code: 400, wantHTTPErr: 1, wantTracked: 0},
		{code: 499, wantHTTPErr: 1, wantTracked: 0},
		{code: 500, wantHTTPErr: 1, wantTracked: 1},
	}
	for _, tc := range tests {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			t.Parallel()

			network := "mut-status-net-" + strconv.Itoa(tc.code)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
			}))
			defer backend.Close()

			p, _, _ := newCBHTTPProxy(t, network, backend.URL)
			httpErr := metrics.ProxyErrors.WithLabelValues(network, "node-1", "rest", strconv.Itoa(tc.code), "http_error")
			httpErrBefore := counterValue(t, httpErr)
			// The same URL is also advertised by an external: its error count
			// shows whether the response was tracked.
			p.endpointStore.StoreAdvertised("ext", "ring", network, "rest", backend.URL)
			p.endpointStore.MarkValidated("ext", "ring", network, "rest", backend.URL, 0, time.Millisecond)

			rr := httptest.NewRecorder()
			p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/status", nil))

			if rr.Code != tc.code {
				t.Fatalf("status = %d, want %d relayed", rr.Code, tc.code)
			}
			if got := counterValue(t, httpErr) - httpErrBefore; got != tc.wantHTTPErr {
				t.Errorf("ProxyErrors{%d,http_error} moved by %v, want %v", tc.code, got, tc.wantHTTPErr)
			}
			eps := p.endpointStore.GetValidatedEndpoints(network, "rest")
			if len(eps) != 1 {
				t.Fatalf("validated endpoints = %d, want 1", len(eps))
			}
			if eps[0].ErrorCount != tc.wantTracked {
				t.Errorf("external endpoint error count = %d, want %d", eps[0].ErrorCount, tc.wantTracked)
			}
		})
	}
}

// Without an external endpoint store a 5xx is relayed as any other response.
func TestMutationKill_HTTPProxy_ServerErrorWithoutEndpointStore(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer backend.Close()

	p, _, _ := newCBHTTPProxy(t, "mut-nostore-net", backend.URL)
	p.endpointStore = nil

	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/status", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 relayed", rr.Code)
	}
}

// A WebSocket that ends cleanly is a success for the node: its error count is
// reset, as for a served HTTP request.
func TestMutationKill_HTTPProxy_WebSocketSuccessResetsBreakerCount(t *testing.T) {
	t.Parallel()

	const network = "mut-wsreset-net"
	backend := wsEchoBackend(t, false)
	p, cb, heightStore := newCBHTTPProxy(t, network, backend.URL)
	heightStore.UpdateWebSocketAvailability(network, "node-1", "rpc", true)
	cb.RecordError(network, "node-1", "rest", 100)
	cb.RecordError(network, "node-1", "rest", 100)

	proxySrv, done := serveSignalingEnd(t, p)
	conn := dialEcho(t, "ws"+proxySrv.URL[len("http"):]+"/ws")
	_ = conn.Close()

	waitHandlerEnd(t, done)
	if got := cb.GetErrorCount(network, "node-1", "rest"); got != 0 {
		t.Errorf("circuit breaker errors after a clean WebSocket = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// WebSocket
// ---------------------------------------------------------------------------

// hijackWriter is a ResponseWriter whose Hijack hands out conn (or fails).
type hijackWriter struct {
	http.ResponseWriter
	conn net.Conn
	err  error
}

func (h *hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h.err != nil {
		return nil, nil, h.err
	}
	return h.conn, bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn)), nil
}

// wsUpgradeRequest is a client WebSocket handshake for requestURI.
func wsUpgradeRequest(requestURI string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, requestURI, nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	return r
}

// pipeClient returns the proxy's end of an in-memory client connection and a
// channel delivering everything the proxy wrote to it once it closes. With
// drain false the far end is closed at once, so every write to it fails.
func pipeClient(drain bool) (net.Conn, <-chan []byte) {
	far, near := net.Pipe()
	out := make(chan []byte, 1)
	if !drain {
		_ = far.Close()
		out <- nil
		return near, out
	}
	go func() {
		b, _ := io.ReadAll(far)
		out <- b
	}()
	return near, out
}

// Only a request that both asks for a connection upgrade and upgrades to
// websocket is a WebSocket request.
func TestMutationKill_IsWebSocketRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		connection string
		upgrade    string
		want       bool
	}{
		{name: "plain upgrade", connection: "Upgrade", upgrade: "websocket", want: true},
		{name: "mixed case", connection: "UPGRADE", upgrade: "WebSocket", want: true},
		{name: "connection list", connection: "keep-alive, Upgrade", upgrade: "websocket", want: true},
		{name: "other protocol", connection: "Upgrade", upgrade: "h2c", want: false},
		{name: "no upgrade header", connection: "Upgrade", upgrade: "", want: false},
		{name: "no connection upgrade", connection: "keep-alive", upgrade: "websocket", want: false},
		{name: "no headers", connection: "", upgrade: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.connection != "" {
				r.Header.Set("Connection", tc.connection)
			}
			if tc.upgrade != "" {
				r.Header.Set("Upgrade", tc.upgrade)
			}
			if got := IsWebSocketRequest(r); got != tc.want {
				t.Errorf("IsWebSocketRequest = %v, want %v", got, tc.want)
			}
		})
	}
}

// A connection that cannot be hijacked, or a backend nobody listens on, end
// the upgrade with a backend_connect_error: status 0 when the client was never
// answered, 502 (also written to the client) when the dial failed.
func TestMutationKill_ProxyWebSocket_ConnectFailures(t *testing.T) {
	t.Parallel()

	t.Run("hijack fails", func(t *testing.T) {
		t.Parallel()
		hijackErr := errors.New("hijack refused")
		w := &hijackWriter{ResponseWriter: httptest.NewRecorder(), err: hijackErr}

		code, err := ProxyWebSocket(w, wsUpgradeRequest("/ws"), "http://"+closedTCPAddr(t), zap.NewNop(), 0)

		var wsErr *WSProxyError
		if !errors.As(err, &wsErr) {
			t.Fatalf("err = %v, want *WSProxyError", err)
		}
		if wsErr.Category != "backend_connect_error" || !errors.Is(err, hijackErr) {
			t.Errorf("category = %q, err = %v, want backend_connect_error wrapping the hijack error", wsErr.Category, err)
		}
		if code != 0 {
			t.Errorf("status = %d, want 0 (nothing was answered)", code)
		}
	})

	t.Run("backend refuses the connection", func(t *testing.T) {
		t.Parallel()
		client, written := pipeClient(true)
		w := &hijackWriter{ResponseWriter: httptest.NewRecorder(), conn: client}

		code, err := ProxyWebSocket(w, wsUpgradeRequest("/ws"), "http://"+closedTCPAddr(t), zap.NewNop(), 0)

		var wsErr *WSProxyError
		if !errors.As(err, &wsErr) {
			t.Fatalf("err = %v, want *WSProxyError", err)
		}
		if wsErr.Category != "backend_connect_error" || wsErr.StatusCode != http.StatusBadGateway {
			t.Errorf("category = %q status = %d, want backend_connect_error 502", wsErr.Category, wsErr.StatusCode)
		}
		if code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", code)
		}
		if got := string(<-written); !strings.HasPrefix(got, "HTTP/1.1 502 Bad Gateway") {
			t.Errorf("client received %q, want a 502 answer", got)
		}
	})
}

// The backend URL for the session is ws:// for an http backend and wss:// for
// an https one, with the request's path and query appended.
func TestMutationKill_ProxyWebSocket_BackendURLLogged(t *testing.T) {
	t.Parallel()

	addr := closedTCPAddr(t) // the dial fails after the URL is logged
	tests := []struct {
		name       string
		backend    string
		requestURI string
		want       string
	}{
		{name: "http with query", backend: "http://" + addr, requestURI: "/ws?a=b", want: "ws://" + addr + "/ws?a=b"},
		{name: "http without query", backend: "http://" + addr, requestURI: "/ws", want: "ws://" + addr + "/ws"},
		{name: "https with query", backend: "https://" + addr, requestURI: "/ws?a=b", want: "wss://" + addr + "/ws?a=b"},
		{name: "https without query", backend: "https://" + addr, requestURI: "/ws", want: "wss://" + addr + "/ws"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logger, logs := debugLogger()
			client, written := pipeClient(true)
			w := &hijackWriter{ResponseWriter: httptest.NewRecorder(), conn: client}

			_, _ = ProxyWebSocket(w, wsUpgradeRequest(tc.requestURI), tc.backend, logger, 0)
			<-written

			entries := logs.FilterMessage("Connecting to backend WebSocket").All()
			if len(entries) != 1 {
				t.Fatalf("connect log entries = %d, want 1", len(entries))
			}
			if got := entries[0].ContextMap()["url"]; got != tc.want {
				t.Errorf("logged url = %v, want %q", got, tc.want)
			}
		})
	}
}

// A refused upgrade is forwarded to the client; failing to write that answer
// is logged, and a healthy client produces no such log.
func TestMutationKill_ProxyWebSocket_RefusedUpgradeForwardFailureLogged(t *testing.T) {
	t.Parallel()

	const msg = "Forwarding a refused WebSocket upgrade ended early"
	tests := []struct {
		name       string
		clientOpen bool
		wantLogged int
	}{
		{name: "client still connected", clientOpen: true, wantLogged: 0},
		{name: "client gone", clientOpen: false, wantLogged: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			}))
			defer backend.Close()
			logger, logs := debugLogger()
			client, written := pipeClient(tc.clientOpen)
			w := &hijackWriter{ResponseWriter: httptest.NewRecorder(), conn: client}

			code, err := ProxyWebSocket(w, wsUpgradeRequest("/ws"), backend.URL, logger, 0)

			var wsErr *WSProxyError
			if !errors.As(err, &wsErr) || wsErr.Category != "upgrade_rejected" {
				t.Fatalf("err = %v, want upgrade_rejected", err)
			}
			if code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", code)
			}
			if tc.clientOpen {
				if got := string(<-written); !strings.HasPrefix(got, "HTTP/1.1 403") {
					t.Errorf("client received %q, want the backend's 403", got)
				}
			}
			if got := logs.FilterMessage(msg).Len(); got != tc.wantLogged {
				t.Errorf("%q logged %d times, want %d", msg, got, tc.wantLogged)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GRPCProxy
// ---------------------------------------------------------------------------

// Only the last slash separates the service from the method; a service name
// that is empty after the leading slash stays empty.
func TestMutationKill_NormalizeGRPCMethod_EmptyService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{input: "//Method", want: ""},
		{input: "/pkg.Svc/Method", want: "pkg.Svc"},
		{input: "/a/b/Method", want: "a/b"},
	}
	for _, tc := range tests {
		if got := normalizeGRPCMethod(tc.input); got != tc.want {
			t.Errorf("normalizeGRPCMethod(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// closedConn returns a connection already closed (state SHUTDOWN), whose Close
// reports it.
func closedConn(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn := dialInsecure(t, addr)
	if err := conn.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	return conn
}

// Close returns the error of a pooled connection that fails to close, logs it
// with the address, and closes the others; a clean Close returns nil quietly.
func TestMutationKill_GRPCProxy_CloseReportsFailedClose(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		alreadyOff bool
		wantErr    bool
		wantWarns  int
	}{
		{name: "healthy connection", alreadyOff: false, wantErr: false, wantWarns: 0},
		{name: "connection already closed", alreadyOff: true, wantErr: true, wantWarns: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newTestGRPCProxy(t)
			logger, logs := debugLogger()
			p.logger = logger
			const addr = "127.0.0.1:19001"
			if tc.alreadyOff {
				p.connPool[addr] = closedConn(t, addr)
			} else {
				p.connPool[addr] = dialInsecure(t, addr)
			}

			err := p.Close()

			if tc.wantErr {
				if status.Code(err) != codes.Canceled {
					t.Errorf("Close = %v, want the Canceled error of a closed connection", err)
				}
			} else if err != nil {
				t.Errorf("Close = %v, want nil", err)
			}
			warns := logs.FilterMessage("Failed to close gRPC connection")
			if warns.Len() != tc.wantWarns {
				t.Fatalf("close warnings = %d, want %d", warns.Len(), tc.wantWarns)
			}
			if tc.wantWarns == 1 && warns.All()[0].ContextMap()["addr"] != addr {
				t.Errorf("warning addr = %v, want %q", warns.All()[0].ContextMap()["addr"], addr)
			}
			if len(p.connPool) != 0 {
				t.Errorf("pool has %d connections after Close, want 0", len(p.connPool))
			}
		})
	}
}

// CloseConnectionsExcept drops every connection keep rejects and counts it,
// including one that fails to close, which is logged with its address.
func TestMutationKill_GRPCProxy_CloseConnectionsExceptReportsFailedClose(t *testing.T) {
	t.Parallel()

	p := newTestGRPCProxy(t)
	logger, logs := debugLogger()
	p.logger = logger
	const dead, live = "127.0.0.1:19002", "127.0.0.1:19003"
	p.connPool[dead] = closedConn(t, dead)
	p.connPool[live] = dialInsecure(t, live)

	n := p.CloseConnectionsExcept(func(string) bool { return false })

	if n != 2 {
		t.Errorf("closed = %d, want 2", n)
	}
	warns := logs.FilterMessage("Failed to close gRPC connection")
	if warns.Len() != 1 {
		t.Fatalf("close warnings = %d, want 1 (only the already-closed connection)", warns.Len())
	}
	if got := warns.All()[0].ContextMap()["addr"]; got != dead {
		t.Errorf("warning addr = %v, want %q", got, dead)
	}
	if addrs := p.PooledAddresses(); len(addrs) != 0 {
		t.Errorf("pooled addresses = %v, want none", addrs)
	}
}

// Replacing a pooled connection that is in SHUTDOWN logs the failure to close
// it, with its address, and still hands out a new connection.
func TestMutationKill_GRPCProxy_ReplacingShutdownConnectionLogsCloseFailure(t *testing.T) {
	t.Parallel()

	p := newTestGRPCProxy(t)
	logger, logs := debugLogger()
	p.logger = logger
	const addr = "127.0.0.1:19004"
	old := closedConn(t, addr)
	p.connPool[grpcutil.ConnKey(addr, true)] = old

	conn, err := p.getOrCreateConnection(addr, true)
	if err != nil {
		t.Fatalf("getOrCreateConnection: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if conn == old {
		t.Error("got the SHUTDOWN connection back, want a new one")
	}
	warns := logs.FilterMessage("Failed to close SHUTDOWN gRPC connection")
	if warns.Len() != 1 {
		t.Fatalf("warnings = %d, want 1", warns.Len())
	}
	if got := warns.All()[0].ContextMap()["addr"]; got != addr {
		t.Errorf("warning addr = %v, want %q", got, addr)
	}
}

// A request the proxy cannot send to the backend (over the backend send limit)
// while the client is still connected is a failure of the proxy, logged at
// Error level.
func TestMutationKill_GRPCProxy_SendFailureWithLiveClientIsProxyError(t *testing.T) {
	t.Parallel()

	core, logs := observer.New(zapcore.ErrorLevel)
	handler := func(_ interface{}, stream grpc.ServerStream) error {
		var f rawFrame
		return stream.RecvMsg(&f)
	}
	h := setupGRPCProxyTestWithOpts(t, handler, grpcProxyTestOpts{maxSend: 1024, logger: zap.New(core)})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := func() error {
		stream, err := h.conn.NewStream(ctx,
			&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
			"/mutsend.Service/Upload",
		)
		if err != nil {
			t.Fatalf("NewStream: %v", err)
		}
		if err := stream.SendMsg(&rawFrame{payload: make([]byte, 4096)}); err != nil {
			t.Fatalf("SendMsg: %v", err)
		}
		_ = stream.CloseSend()
		var f rawFrame
		return stream.RecvMsg(&f)
	}()

	if status.Code(res) != codes.ResourceExhausted {
		t.Fatalf("code = %s, want ResourceExhausted", status.Code(res))
	}
	if got := logs.FilterMessage("Error sending to backend").Len(); got != 1 {
		t.Errorf("'Error sending to backend' logged %d times, want 1", got)
	}
}

// A backend answering a node-failure status counts against the external
// endpoint that advertises that address.
func TestMutationKill_GRPCProxy_BackendFailureTrackedForExternalEndpoint(t *testing.T) {
	t.Parallel()

	handler := func(_ interface{}, stream grpc.ServerStream) error {
		var f rawFrame
		if err := stream.RecvMsg(&f); err != nil {
			return err
		}
		return status.Error(codes.Unavailable, "node is broken")
	}
	h := setupGRPCProxyTest(t, handler)
	addr := h.proxy.selector.GetEndpointURL("node-1", "grpc")
	h.proxy.endpointStore.StoreAdvertised("ext", "ring", "testnet", "grpc", addr)
	h.proxy.endpointStore.MarkValidated("ext", "ring", "testnet", "grpc", addr, 0, time.Millisecond)

	res := rawCall(t, h.conn, "/muttrack.Service/Query")
	if status.Code(res.err) != codes.Unavailable {
		t.Fatalf("err = %v, want Unavailable", res.err)
	}

	eps := h.proxy.endpointStore.GetValidatedEndpoints("testnet", "grpc")
	if len(eps) != 1 {
		t.Fatalf("validated endpoints = %d, want 1", len(eps))
	}
	if eps[0].ErrorCount != 1 {
		t.Errorf("external endpoint error count = %d, want 1", eps[0].ErrorCount)
	}
}

// A backend that cannot be reached fails the call with Unavailable and counts
// against both the node's circuit breaker and the external endpoint.
func TestMutationKill_GRPCProxy_UnreachableBackendCountedAgainstBreakerAndEndpoint(t *testing.T) {
	t.Parallel()

	const network = "mut-grpc-dead-net"
	deadAddr := closedTCPAddr(t)
	yaml := `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 5s
networks:
  - name: ` + network + `
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rpc
        listen: ":18081"
      - protocol: grpc
        listen: ":18082"
    circuit_breaker:
      http_codes: [502, 503]
      threshold: 100
      retry_attempts: 0
internals:
  - name: node-a
    network: ` + network + `
    grpc_insecure: true
    endpoints:
      rpc: "http://127.0.0.1:18099"
      grpc: "` + deadAddr + `"
`
	cfgLoader := testutil.NewMultiChainLoader(t, yaml)
	heightStore := storage.NewHeightStore()
	heightStore.Update(network, "node-a", "rpc", 100, time.Millisecond, "internal")
	healthStore := storage.NewHealthStore()
	healthStore.SetHealthy(network, "node-a", "grpc")
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	endpointStore.StoreAdvertised("ext", "ring", network, "grpc", deadAddr)
	endpointStore.MarkValidated("ext", "ring", network, "grpc", deadAddr, 0, time.Millisecond)
	sel := selector.NewSelector(heightStore, endpointStore, cfgLoader, zap.NewNop())
	sel.SetHealthStore(healthStore)
	cb := NewCircuitBreaker(healthStore, zap.NewNop())
	grpcProxy := NewGRPCProxy(sel, cfgLoader, endpointStore, zap.NewNop(), network, cb)
	t.Cleanup(func() { _ = grpcProxy.Close() })

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	srv := grpcProxy.GetServer()
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///"+lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(&rawCodec{})),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	res := rawCall(t, conn, "/mutdead.Service/Query")

	if status.Code(res.err) != codes.Unavailable {
		t.Fatalf("err = %v, want Unavailable", res.err)
	}
	if got := cb.GetErrorCount(network, "node-a", "grpc"); got != 1 {
		t.Errorf("circuit breaker errors = %d, want 1", got)
	}
	eps := endpointStore.GetValidatedEndpoints(network, "grpc")
	if len(eps) != 1 {
		t.Fatalf("validated endpoints = %d, want 1", len(eps))
	}
	if eps[0].ErrorCount != 1 {
		t.Errorf("external endpoint error count = %d, want 1", eps[0].ErrorCount)
	}
}
