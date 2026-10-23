package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sauron/metrics"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// ---------------------------------------------------------------------------
// Response metadata and status passthrough
//
// Each test runs the same backend handler twice: once dialled directly and
// once through the proxy. The proxy is transparent only if both calls observe
// the same headers, trailers, messages and status.
// ---------------------------------------------------------------------------

// rawCallResult captures everything a client can observe from one call.
type rawCallResult struct {
	header   metadata.MD
	trailer  metadata.MD
	messages []string
	err      error
}

// rawCall opens a stream, sends one frame, half-closes and drains the reply.
func rawCall(t *testing.T, conn *grpc.ClientConn, method string) rawCallResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := conn.NewStream(ctx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		method,
	)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	if err := stream.SendMsg(&rawFrame{payload: []byte("req")}); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	var res rawCallResult
	for {
		var frame rawFrame
		err := stream.RecvMsg(&frame)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			res.err = err
			break
		}
		res.messages = append(res.messages, string(frame.payload))
	}
	// Header and Trailer are valid once RecvMsg has returned a non-nil error.
	res.header, _ = stream.Header()
	res.trailer = stream.Trailer()
	return res
}

// startDirectBackend serves handler on a random port and returns a client
// connected straight to it — the control that the proxy must match.
func startDirectBackend(t *testing.T, handler grpc.StreamHandler) *grpc.ClientConn {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("direct backend listen: %v", err)
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(handler), grpc.ForceServerCodec(&rawCodec{}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///"+lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(&rawCodec{})),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// mdValue returns the first value of key in md, or "" when absent.
func mdValue(md metadata.MD, key string) string {
	if vals := md.Get(key); len(vals) > 0 {
		return vals[0]
	}
	return ""
}

func TestGRPCProxy_ResponseMetadataPassthrough(t *testing.T) {
	t.Parallel()

	detail := durationpb.New(1500 * time.Millisecond)
	backendStatus, err := status.New(codes.FailedPrecondition,
		"current block height (748062) is less than session's earliest proof commit height (748063)").
		WithDetails(detail)
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}

	tests := []struct {
		name    string
		handler grpc.StreamHandler

		wantHeader   string // x-cosmos-block-height in the header, "" = absent
		wantTrailer  string // x-probe-trailer in the trailer, "" = absent
		wantMessages []string
		wantStatus   *status.Status // nil = call succeeds
	}{
		{
			name: "unary with header and trailer",
			handler: func(_ interface{}, stream grpc.ServerStream) error {
				var f rawFrame
				if err := stream.RecvMsg(&f); err != nil {
					return err
				}
				if err := stream.SetHeader(metadata.Pairs("x-cosmos-block-height", "974831")); err != nil {
					return err
				}
				stream.SetTrailer(metadata.Pairs("x-probe-trailer", "t1"))
				return stream.SendMsg(&rawFrame{payload: []byte("ok")})
			},
			wantHeader:   "974831",
			wantTrailer:  "t1",
			wantMessages: []string{"ok"},
		},
		{
			name: "server streaming keeps header, every message and trailer",
			handler: func(_ interface{}, stream grpc.ServerStream) error {
				var f rawFrame
				if err := stream.RecvMsg(&f); err != nil {
					return err
				}
				if err := stream.SendHeader(metadata.Pairs("x-cosmos-block-height", "752797")); err != nil {
					return err
				}
				for _, m := range []string{"m1", "m2", "m3"} {
					if err := stream.SendMsg(&rawFrame{payload: []byte(m)}); err != nil {
						return err
					}
				}
				stream.SetTrailer(metadata.Pairs("x-probe-trailer", "t-stream"))
				return nil
			},
			wantHeader:   "752797",
			wantTrailer:  "t-stream",
			wantMessages: []string{"m1", "m2", "m3"},
		},
		{
			name: "header set but no messages, success",
			handler: func(_ interface{}, stream grpc.ServerStream) error {
				var f rawFrame
				if err := stream.RecvMsg(&f); err != nil {
					return err
				}
				return stream.SetHeader(metadata.Pairs("x-cosmos-block-height", "1"))
			},
			wantHeader: "1",
		},
		{
			name: "trailers-only error keeps code, message, details and trailer",
			handler: func(_ interface{}, stream grpc.ServerStream) error {
				var f rawFrame
				if err := stream.RecvMsg(&f); err != nil {
					return err
				}
				stream.SetTrailer(metadata.Pairs("x-probe-trailer", "t-err"))
				return backendStatus.Err()
			},
			wantTrailer: "t-err",
			wantStatus:  backendStatus,
		},
		{
			name: "error after header and a message keeps all three",
			handler: func(_ interface{}, stream grpc.ServerStream) error {
				var f rawFrame
				if err := stream.RecvMsg(&f); err != nil {
					return err
				}
				if err := stream.SendHeader(metadata.Pairs("x-cosmos-block-height", "42")); err != nil {
					return err
				}
				if err := stream.SendMsg(&rawFrame{payload: []byte("partial")}); err != nil {
					return err
				}
				return status.Error(codes.NotFound, "claim not found")
			},
			wantHeader:   "42",
			wantMessages: []string{"partial"},
			wantStatus:   status.New(codes.NotFound, "claim not found"),
		},
		{
			name: "empty message in backend status",
			handler: func(_ interface{}, stream grpc.ServerStream) error {
				var f rawFrame
				if err := stream.RecvMsg(&f); err != nil {
					return err
				}
				return status.Error(codes.InvalidArgument, "")
			},
			wantStatus: status.New(codes.InvalidArgument, ""),
		},
		{
			name: "unicode message in backend status",
			handler: func(_ interface{}, stream grpc.ServerStream) error {
				var f rawFrame
				if err := stream.RecvMsg(&f); err != nil {
					return err
				}
				return status.Error(codes.Unavailable, "nodo caído: altura ≥ 974831 — 再試行")
			},
			wantStatus: status.New(codes.Unavailable, "nodo caído: altura ≥ 974831 — 再試行"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			direct := rawCall(t, startDirectBackend(t, tc.handler), "/pocket.shared.Query/Params")
			proxied := rawCall(t, setupGRPCProxyTest(t, tc.handler).conn, "/pocket.shared.Query/Params")

			for _, side := range []struct {
				label string
				res   rawCallResult
			}{{"direct", direct}, {"proxied", proxied}} {
				res := side.res

				if got := mdValue(res.header, "x-cosmos-block-height"); got != tc.wantHeader {
					t.Errorf("%s: header x-cosmos-block-height = %q, want %q", side.label, got, tc.wantHeader)
				}
				if got := mdValue(res.trailer, "x-probe-trailer"); got != tc.wantTrailer {
					t.Errorf("%s: trailer x-probe-trailer = %q, want %q", side.label, got, tc.wantTrailer)
				}
				if len(res.messages) != len(tc.wantMessages) {
					t.Errorf("%s: messages = %q, want %q", side.label, res.messages, tc.wantMessages)
				} else {
					for i := range res.messages {
						if res.messages[i] != tc.wantMessages[i] {
							t.Errorf("%s: message[%d] = %q, want %q", side.label, i, res.messages[i], tc.wantMessages[i])
						}
					}
				}

				if tc.wantStatus == nil {
					if res.err != nil {
						t.Errorf("%s: unexpected error: %v", side.label, res.err)
					}
					continue
				}
				got, ok := status.FromError(res.err)
				if !ok {
					t.Errorf("%s: error is not a gRPC status: %v", side.label, res.err)
					continue
				}
				if !proto.Equal(got.Proto(), tc.wantStatus.Proto()) {
					t.Errorf("%s: status = {code=%s msg=%q details=%d}, want {code=%s msg=%q details=%d}",
						side.label,
						got.Code(), got.Message(), len(got.Details()),
						tc.wantStatus.Code(), tc.wantStatus.Message(), len(tc.wantStatus.Details()))
				}
			}
		})
	}
}

// TestGRPCProxy_ClientCancelMidStream cancels the client while the backend is
// still streaming. The proxy must propagate the cancel to the backend and keep
// serving later calls; run with -race to catch the handler returning while its
// server->client goroutine still touches the server stream.
func TestGRPCProxy_ClientCancelMidStream(t *testing.T) {
	t.Parallel()

	backendDone := make(chan error, 1)
	handler := func(_ interface{}, stream grpc.ServerStream) error {
		var f rawFrame
		if err := stream.RecvMsg(&f); err != nil {
			return err
		}
		if string(f.payload) == "ping" {
			stream.SetTrailer(metadata.Pairs("x-probe-trailer", "alive"))
			return stream.SendMsg(&rawFrame{payload: []byte("pong")})
		}
		_ = stream.SetHeader(metadata.Pairs("x-cosmos-block-height", "7"))
		for {
			if err := stream.SendMsg(&rawFrame{payload: []byte("tick")}); err != nil {
				backendDone <- err
				return err
			}
			select {
			case <-stream.Context().Done():
				backendDone <- stream.Context().Err()
				return stream.Context().Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}

	h := setupGRPCProxyTest(t, handler)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := h.conn.NewStream(ctx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		"/test.Service/Ticker",
	)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	if err := stream.SendMsg(&rawFrame{payload: []byte("start")}); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	for i := 0; i < 3; i++ {
		var f rawFrame
		if err := stream.RecvMsg(&f); err != nil {
			t.Fatalf("RecvMsg %d: %v", i, err)
		}
	}
	cancel()

	select {
	case err := <-backendDone:
		if err == nil {
			t.Error("backend stream ended without error after client cancel")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("backend never observed the client cancel")
	}

	// The proxy must still serve a fresh call, metadata included.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	s2, err := h.conn.NewStream(ctx2,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		"/test.Service/Ticker",
	)
	if err != nil {
		t.Fatalf("NewStream after cancel: %v", err)
	}
	if err := s2.SendMsg(&rawFrame{payload: []byte("ping")}); err != nil {
		t.Fatalf("SendMsg after cancel: %v", err)
	}
	_ = s2.CloseSend()
	var f rawFrame
	if err := s2.RecvMsg(&f); err != nil {
		t.Fatalf("RecvMsg after cancel: %v", err)
	}
	if string(f.payload) != "pong" {
		t.Errorf("payload after cancel = %q, want %q", f.payload, "pong")
	}
	if err := s2.RecvMsg(&f); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF after pong, got %v", err)
	}
	if got := mdValue(s2.Trailer(), "x-probe-trailer"); got != "alive" {
		t.Errorf("trailer after cancel = %q, want %q", got, "alive")
	}
}

// handlerFinished reports whether proxyHandler reached its metrics block for
// service, i.e. returned, by reading the per-service request counter.
func handlerFinished(service string) bool {
	var m dto.Metric
	if err := metrics.NodeRequests.WithLabelValues("testnet", "node-1", "grpc", service).Write(&m); err != nil {
		return false
	}
	return m.GetCounter().GetValue() >= 1
}

// waitHandlerFinished polls handlerFinished until it holds or timeout passes.
func waitHandlerFinished(service string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if handlerFinished(service) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return handlerFinished(service)
}

// TestGRPCProxy_ReturnsWhenClientStallsAndRequestFails: the client stops
// reading a server stream (so the proxy's send to it blocks on flow control)
// and then sends a frame the proxy cannot forward (over its send limit to the
// backend). The client stream stays healthy, so nothing resets it; the proxy
// must still finish the call instead of waiting on the blocked send.
func TestGRPCProxy_ReturnsWhenClientStallsAndRequestFails(t *testing.T) {
	t.Parallel()

	const service = "stall.Service"
	chunk := make([]byte, 1000)
	var lastSend atomic.Int64 // unix nanos of the backend's last completed send
	handler := func(_ interface{}, stream grpc.ServerStream) error {
		var f rawFrame
		if err := stream.RecvMsg(&f); err != nil {
			return err
		}
		for {
			if err := stream.SendMsg(&rawFrame{payload: chunk}); err != nil {
				return err
			}
			lastSend.Store(time.Now().UnixNano())
		}
	}

	h := setupGRPCProxyTestWithOpts(t, handler, grpcProxyTestOpts{maxSend: 1024})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := h.conn.NewStream(ctx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		"/"+service+"/Stall",
	)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	if err := stream.SendMsg(&rawFrame{payload: []byte("start")}); err != nil {
		t.Fatalf("SendMsg start: %v", err)
	}

	// Never read: once the proxy->client window is full the proxy's send blocks,
	// and backpressure then stalls the backend's sends too.
	deadline := time.Now().Add(15 * time.Second)
	for {
		last := lastSend.Load()
		if last != 0 && time.Since(time.Unix(0, last)) > 300*time.Millisecond {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("flow control never stalled the backend; test precondition not met")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Over the 1024-byte send limit: the proxy's SendMsg to the backend fails.
	if err := stream.SendMsg(&rawFrame{payload: make([]byte, 4096)}); err != nil {
		t.Fatalf("SendMsg oversized: %v", err)
	}

	if !waitHandlerFinished(service, 3*time.Second) {
		t.Fatal("proxy handler did not return after the client request failed while its send was blocked")
	}
}

// TestGRPCProxy_BackendEndsBeforeClientHalfClose: the backend replies and ends
// the call while the client has not half-closed (a bidi client waiting for the
// status). The client must get the reply, the trailer and a clean end of
// stream, not hang until its deadline.
func TestGRPCProxy_BackendEndsBeforeClientHalfClose(t *testing.T) {
	t.Parallel()

	handler := func(_ interface{}, stream grpc.ServerStream) error {
		var f rawFrame
		if err := stream.RecvMsg(&f); err != nil {
			return err
		}
		stream.SetTrailer(metadata.Pairs("x-probe-trailer", "done"))
		return stream.SendMsg(&rawFrame{payload: []byte("reply")})
	}

	h := setupGRPCProxyTest(t, handler)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := h.conn.NewStream(ctx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		"/bidi.Service/NoHalfClose",
	)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	if err := stream.SendMsg(&rawFrame{payload: []byte("hi")}); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	// Deliberately no CloseSend.

	var f rawFrame
	if err := stream.RecvMsg(&f); err != nil {
		t.Fatalf("RecvMsg reply: %v", err)
	}
	if string(f.payload) != "reply" {
		t.Errorf("reply = %q, want %q", f.payload, "reply")
	}
	if err := stream.RecvMsg(&f); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream = %v, want io.EOF", err)
	}
	if got := mdValue(stream.Trailer(), "x-probe-trailer"); got != "done" {
		t.Errorf("trailer = %q, want %q", got, "done")
	}
}

// TestGRPCProxy_NoErrorLogsForRelayedBackendStatus: a backend status (here
// NotFound, before the client half-closes) is a normal reply. Relaying it must
// not produce Error-level logs, which the per-request path reserves for
// failures of the proxy itself.
func TestGRPCProxy_NoErrorLogsForRelayedBackendStatus(t *testing.T) {
	t.Parallel()

	core, logs := observer.New(zapcore.ErrorLevel)
	handler := func(_ interface{}, stream grpc.ServerStream) error {
		var f rawFrame
		if err := stream.RecvMsg(&f); err != nil {
			return err
		}
		return status.Error(codes.NotFound, "claim not found")
	}
	h := setupGRPCProxyTestWithOpts(t, handler, grpcProxyTestOpts{logger: zap.New(core)})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := h.conn.NewStream(ctx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		"/quiet.Service/NotFound",
	)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	if err := stream.SendMsg(&rawFrame{payload: []byte("q")}); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	var f rawFrame
	if err := stream.RecvMsg(&f); status.Code(err) != codes.NotFound {
		t.Fatalf("RecvMsg = %v, want NotFound", err)
	}
	cancel() // the client walks away, as a real one does after the status

	if !waitHandlerFinished("quiet.Service", 3*time.Second) {
		t.Fatal("proxy handler did not finish")
	}
	time.Sleep(100 * time.Millisecond) // let the client->server goroutine wind down

	if n := logs.Len(); n != 0 {
		for _, e := range logs.All() {
			t.Errorf("unexpected Error log: %q %v", e.Message, e.ContextMap())
		}
	}
}

// proxyErrCount reads ProxyErrors for the shared test node.
func proxyErrCount(code codes.Code, reason string) float64 {
	var m dto.Metric
	c := metrics.ProxyErrors.WithLabelValues("testnet", "node-1", "grpc", strconv.Itoa(int(code)), reason)
	if err := c.Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// TestGRPCProxy_ClientCancelLogsNoErrors: a client cancelling a call while the
// backend is still working (a slow query) is routine and must not produce
// Error-level logs. Which forwarding goroutine notices the cancel first is a
// race, so the call is repeated to cover both orders.
func TestGRPCProxy_ClientCancelLogsNoErrors(t *testing.T) {
	t.Parallel()

	const service = "cancel.Service"
	const calls = 20
	core, logs := observer.New(zapcore.ErrorLevel)
	handler := func(_ interface{}, stream grpc.ServerStream) error {
		var f rawFrame
		if err := stream.RecvMsg(&f); err != nil {
			return err
		}
		<-stream.Context().Done() // a slow query: never answers before the cancel
		return stream.Context().Err()
	}
	h := setupGRPCProxyTestWithOpts(t, handler, grpcProxyTestOpts{logger: zap.New(core)})

	for i := 0; i < calls; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		stream, err := h.conn.NewStream(ctx,
			&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
			"/"+service+"/Slow",
		)
		if err != nil {
			t.Fatalf("NewStream: %v", err)
		}
		if err := stream.SendMsg(&rawFrame{payload: []byte("q")}); err != nil {
			t.Fatalf("SendMsg: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
		cancel()
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		var m dto.Metric
		_ = metrics.NodeRequests.WithLabelValues("testnet", "node-1", "grpc", service).Write(&m)
		if m.GetCounter().GetValue() >= calls {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %v of %d calls finished", m.GetCounter().GetValue(), calls)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // let the forwarding goroutines wind down

	for _, e := range logs.All() {
		t.Errorf("unexpected Error log: %q %v", e.Message, e.ContextMap())
	}
}

// TestGRPCProxy_ProxySideErrorKeepsCleanMessage: an error raised on the proxy
// side (here, a request over the backend send limit) must reach the client as
// a plain gRPC status, without the proxy's internal "send to backend:" wording.
func TestGRPCProxy_ProxySideErrorKeepsCleanMessage(t *testing.T) {
	t.Parallel()

	handler := func(_ interface{}, stream grpc.ServerStream) error {
		var f rawFrame
		return stream.RecvMsg(&f)
	}
	h := setupGRPCProxyTestWithOpts(t, handler, grpcProxyTestOpts{maxSend: 1024})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := h.conn.NewStream(ctx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		"/big.Service/Upload",
	)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	if err := stream.SendMsg(&rawFrame{payload: make([]byte, 4096)}); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	_ = stream.CloseSend()
	var f rawFrame
	err = stream.RecvMsg(&f)

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a gRPC status: %v", err)
	}
	if st.Code() != codes.ResourceExhausted {
		t.Errorf("code = %s, want ResourceExhausted", st.Code())
	}
	for _, internal := range []string{"send to backend", "recv from client", "send to client", "rpc error:"} {
		if strings.Contains(st.Message(), internal) {
			t.Errorf("message %q leaks proxy-internal wording %q", st.Message(), internal)
		}
	}
}

// TestGRPCProxy_ProxyErrorsReasonByOrigin: ProxyErrors must tell a relayed
// backend status apart from a failure of the proxy. Not parallel: it reads
// global counters that parallel tests would also move.
func TestGRPCProxy_ProxyErrorsReasonByOrigin(t *testing.T) {
	handler := func(_ interface{}, stream grpc.ServerStream) error {
		var f rawFrame
		if err := stream.RecvMsg(&f); err != nil {
			return err
		}
		return status.Error(codes.NotFound, "claim not found")
	}
	h := setupGRPCProxyTest(t, handler)

	backendBefore := proxyErrCount(codes.NotFound, "backend_status")
	proxyBefore := proxyErrCount(codes.NotFound, "proxy_error")

	res := rawCall(t, h.conn, "/reason.Service/NotFound")
	if status.Code(res.err) != codes.NotFound {
		t.Fatalf("err = %v, want NotFound", res.err)
	}
	if !waitHandlerFinished("reason.Service", 3*time.Second) {
		t.Fatal("proxy handler did not finish")
	}

	if got := proxyErrCount(codes.NotFound, "backend_status") - backendBefore; got != 1 {
		t.Errorf("ProxyErrors{reason=backend_status} delta = %v, want 1", got)
	}
	if got := proxyErrCount(codes.NotFound, "proxy_error") - proxyBefore; got != 0 {
		t.Errorf("ProxyErrors{reason=proxy_error} delta = %v, want 0", got)
	}
}

// TestTrackableBackendFailure: only failures that say something about the
// backend may count against an external endpoint's health. A client going
// away surfaces as Canceled, Unavailable or Internal too, and must not.
func TestTrackableBackendFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		origin string
		code   codes.Code
		want   bool
	}{
		{originBackend, codes.Unavailable, true},
		{originBackend, codes.Internal, true},
		{originBackend, codes.Unknown, true},
		{originBackend, codes.DataLoss, true},
		{originBackend, codes.NotFound, false},
		{originBackend, codes.OK, false},
		{originProxy, codes.Unavailable, true},
		{originProxy, codes.ResourceExhausted, false},
		{originClient, codes.Unavailable, false},
		{originClient, codes.Internal, false},
		{originClient, codes.Canceled, false},
		{originClient, codes.Unknown, false},
	}
	for _, tc := range tests {
		if got := trackableBackendFailure(tc.origin, tc.code); got != tc.want {
			t.Errorf("trackableBackendFailure(%s, %s) = %v, want %v", tc.origin, tc.code, got, tc.want)
		}
	}
}

// TestGRPCProxy_ClientConnDropIsClientOrigin: a client whose connection dies
// mid-stream must end as client_error whatever code the transport reports, so
// it never reaches external-endpoint health tracking. Not parallel: it reads
// global counters.
func TestGRPCProxy_ClientConnDropIsClientOrigin(t *testing.T) {
	handler := func(_ interface{}, stream grpc.ServerStream) error {
		var f rawFrame
		if err := stream.RecvMsg(&f); err != nil {
			return err
		}
		for {
			if err := stream.SendMsg(&rawFrame{payload: []byte("tick")}); err != nil {
				return err
			}
			select {
			case <-stream.Context().Done():
				return stream.Context().Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	h := setupGRPCProxyTest(t, handler)

	const service = "drop.Service"
	backendBefore := 0.0
	for _, c := range []codes.Code{codes.Canceled, codes.Unavailable, codes.Internal, codes.Unknown} {
		backendBefore += proxyErrCount(c, originBackend) + proxyErrCount(c, originProxy)
	}

	for i := 0; i < 5; i++ {
		// A dedicated client connection, closed under the live stream.
		conn := dialInsecure(t, h.addr)
		stream, err := conn.NewStream(context.Background(),
			&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
			"/"+service+"/Ticks",
			grpc.ForceCodec(&rawCodec{}),
		)
		if err != nil {
			t.Fatalf("NewStream: %v", err)
		}
		if err := stream.SendMsg(&rawFrame{payload: []byte("go")}); err != nil {
			t.Fatalf("SendMsg: %v", err)
		}
		var f rawFrame
		if err := stream.RecvMsg(&f); err != nil {
			t.Fatalf("RecvMsg: %v", err)
		}
		_ = conn.Close()
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		var m dto.Metric
		_ = metrics.NodeRequests.WithLabelValues("testnet", "node-1", "grpc", service).Write(&m)
		if m.GetCounter().GetValue() >= 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %v of 5 calls finished", m.GetCounter().GetValue())
		}
		time.Sleep(20 * time.Millisecond)
	}

	backendAfter := 0.0
	for _, c := range []codes.Code{codes.Canceled, codes.Unavailable, codes.Internal, codes.Unknown} {
		backendAfter += proxyErrCount(c, originBackend) + proxyErrCount(c, originProxy)
	}
	if d := backendAfter - backendBefore; d != 0 {
		t.Errorf("%v client connection drops were attributed to the backend or the proxy, want 0", d)
	}
}

// TestGRPCProxy_ProxyRecvLimitIsProxyOrigin: a request rejected by the
// proxy's own grpc_max_recv_msg_size is a proxy configuration limit, not a
// client fault, and must surface as proxy_error. Not parallel: global counters.
func TestGRPCProxy_ProxyRecvLimitIsProxyOrigin(t *testing.T) {
	handler := func(_ interface{}, stream grpc.ServerStream) error {
		var f rawFrame
		return stream.RecvMsg(&f)
	}
	h := setupGRPCProxyTestWithOpts(t, handler, grpcProxyTestOpts{maxRecv: 1024})

	before := proxyErrCount(codes.ResourceExhausted, originProxy)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := h.conn.NewStream(ctx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		"/limit.Service/Big",
	)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	if err := stream.SendMsg(&rawFrame{payload: make([]byte, 4096)}); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	_ = stream.CloseSend()
	var f rawFrame
	if err := stream.RecvMsg(&f); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("err = %v, want ResourceExhausted", err)
	}
	if !waitHandlerFinished("limit.Service", 3*time.Second) {
		t.Fatal("proxy handler did not finish")
	}
	if d := proxyErrCount(codes.ResourceExhausted, originProxy) - before; d != 1 {
		t.Errorf("ProxyErrors{8, proxy_error} delta = %v, want 1", d)
	}
}

// TestGRPCProxy_JoinsS2CWhenClientGone: when the client's stream is already
// over, the handler must wait for its server->client goroutine before
// returning, so nothing touches the server stream after the handler returns.
// The goroutine's exit log must therefore come before the call's end log.
// Which side notices a client cancel first is a race, so it is repeated.
func TestGRPCProxy_JoinsS2CWhenClientGone(t *testing.T) {
	t.Parallel()

	core, logs := observer.New(zapcore.DebugLevel)
	handler := func(_ interface{}, stream grpc.ServerStream) error {
		var f rawFrame
		if err := stream.RecvMsg(&f); err != nil {
			return err
		}
		for {
			if err := stream.SendMsg(&rawFrame{payload: make([]byte, 512)}); err != nil {
				return err
			}
		}
	}
	h := setupGRPCProxyTestWithOpts(t, handler, grpcProxyTestOpts{logger: zap.New(core)})

	const calls = 20
	for i := 0; i < calls; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		stream, err := h.conn.NewStream(ctx,
			&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
			"/join.Service/Flood",
		)
		if err != nil {
			t.Fatalf("NewStream: %v", err)
		}
		if err := stream.SendMsg(&rawFrame{payload: []byte("go")}); err != nil {
			t.Fatalf("SendMsg: %v", err)
		}
		var f rawFrame
		if err := stream.RecvMsg(&f); err != nil {
			t.Fatalf("RecvMsg: %v", err)
		}
		cancel()

		// Wait for this call's end log.
		deadline := time.Now().Add(3 * time.Second)
		for logs.FilterMessage("gRPC call ended with status").Len() < i+1 {
			if time.Now().After(deadline) {
				t.Fatalf("call %d never ended", i)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Per call, in order: the s2c exit must be logged before the end log.
	exits, ends := 0, 0
	for _, e := range logs.All() {
		switch e.Message {
		case "Exiting server->client forwarding goroutine":
			exits++
		case "gRPC call ended with status":
			ends++
			if exits < ends {
				t.Errorf("call %d ended before its server->client goroutine exited", ends)
			}
		}
	}
}
