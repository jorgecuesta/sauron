package proxy

import (
	"context"
	"errors"
	"io"
	"net"
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
	srv := grpc.NewServer(grpc.UnknownServiceHandler(handler))
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
