package checker

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"sauron/config"
	"sauron/internal/grpcutil"
)

// grpcAnswerBackend is a gRPC server whose every method answers with code
// (OK sends an empty message). hang makes it never answer. It records the
// methods it was called with.
type grpcAnswerBackend struct {
	addr    string
	mu      sync.Mutex
	code    codes.Code
	hang    bool
	methods []string
}

func newGRPCAnswerBackend(t *testing.T, code codes.Code, hang bool) *grpcAnswerBackend {
	t.Helper()
	b := &grpcAnswerBackend{code: code, hang: hang}
	stop := make(chan struct{})
	handler := func(_ interface{}, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
		var req emptypb.Empty
		if err := stream.RecvMsg(&req); err != nil {
			return err
		}
		b.mu.Lock()
		b.methods = append(b.methods, method)
		code, hang := b.code, b.hang
		b.mu.Unlock()
		if hang {
			select {
			case <-stream.Context().Done():
			case <-stop:
			}
			return stream.Context().Err()
		}
		if code != codes.OK {
			return status.Error(code, "backend says "+code.String())
		}
		return stream.SendMsg(&emptypb.Empty{})
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(handler))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { close(stop); srv.Stop() })
	b.addr = lis.Addr().String()
	return b
}

func (b *grpcAnswerBackend) set(code codes.Code) {
	b.mu.Lock()
	b.code = code
	b.mu.Unlock()
}

func (b *grpcAnswerBackend) calls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.methods...)
}

func runGRPCHealthCheck(t *testing.T, sched *MultiChainScheduler, addr string) {
	t.Helper()
	node := config.MultiChainNode{
		Name:         "test-node",
		Network:      "testnet",
		Endpoints:    map[string]string{"grpc": addr},
		GRPCInsecure: true,
	}
	// drainTestPool stops the pool, so each check gets a fresh one.
	sched.pool = newTestPool()
	sched.scheduleGRPCHealthCheck(node, addr, "grpc")
	drainTestPool(sched.pool)
}

// TestScheduleGRPCHealthCheck_RealQueryVerdict: a node whose transport is READY
// but whose queries fail with a node-failure status is unhealthy; any other
// answer (including NotFound, or Unimplemented on a non-Cosmos chain) proves it
// serves. A query that never answers is unhealthy by the check timeout.
func TestScheduleGRPCHealthCheck_RealQueryVerdict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		code        codes.Code
		hang        bool
		wantHealthy bool
	}{
		{name: "answers OK", code: codes.OK, wantHealthy: true},
		{name: "Unavailable", code: codes.Unavailable, wantHealthy: false},
		{name: "Internal", code: codes.Internal, wantHealthy: false},
		{name: "Unknown", code: codes.Unknown, wantHealthy: false},
		{name: "DataLoss", code: codes.DataLoss, wantHealthy: false},
		{name: "NotFound is an answer", code: codes.NotFound, wantHealthy: true},
		{name: "Unimplemented is an answer", code: codes.Unimplemented, wantHealthy: true},
		{name: "query hangs", hang: true, wantHealthy: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backend := newGRPCAnswerBackend(t, tt.code, tt.hang)
			sched, healthStore := testHealthCheckScheduler(t)
			sched.timeout.Store(int64(500 * time.Millisecond))

			runGRPCHealthCheck(t, sched, backend.addr)

			if got := healthStore.IsHealthy("testnet", "test-node", "grpc"); got != tt.wantHealthy {
				h := healthStore.GetHealth("testnet", "test-node", "grpc")
				t.Fatalf("healthy = %v, want %v (health %+v)", got, tt.wantHealthy, h)
			}
			if !tt.wantHealthy {
				if h := healthStore.GetHealth("testnet", "test-node", "grpc"); h == nil || h.LastError == "" {
					t.Errorf("unhealthy without LastError: %+v", h)
				}
			}
			calls := backend.calls()
			if len(calls) != 1 || calls[0] != grpcHealthQueryMethod {
				t.Errorf("backend calls = %v, want exactly one %s", calls, grpcHealthQueryMethod)
			}
		})
	}
}

// A node that recovers is marked healthy by the next check, and one that
// breaks is marked unhealthy, without any client traffic.
func TestScheduleGRPCHealthCheck_TransitionsWithoutTraffic(t *testing.T) {
	t.Parallel()
	backend := newGRPCAnswerBackend(t, codes.Unavailable, false)
	sched, healthStore := testHealthCheckScheduler(t)

	runGRPCHealthCheck(t, sched, backend.addr)
	if healthStore.IsHealthy("testnet", "test-node", "grpc") {
		t.Fatal("broken node healthy after the first check")
	}
	runGRPCHealthCheck(t, sched, backend.addr)
	if healthStore.IsHealthy("testnet", "test-node", "grpc") {
		t.Fatal("still-broken node re-included by the second check")
	}

	backend.set(codes.OK)
	runGRPCHealthCheck(t, sched, backend.addr)
	if !healthStore.IsHealthy("testnet", "test-node", "grpc") {
		t.Fatalf("recovered node not re-included: %+v", healthStore.GetHealth("testnet", "test-node", "grpc"))
	}

	backend.set(codes.Internal)
	runGRPCHealthCheck(t, sched, backend.addr)
	if healthStore.IsHealthy("testnet", "test-node", "grpc") {
		t.Fatal("node that broke again still healthy")
	}
}

// The check only asks whether the node answers: it does not decode the
// answer, so a reply that is not the message the check would expect (here,
// raw bytes that are not valid protobuf) still proves the node serves. A local
// decoding failure must never mark a node unhealthy.
func TestScheduleGRPCHealthCheck_AnswerIsNotDecoded(t *testing.T) {
	t.Parallel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.ForceServerCodec(grpcutil.BytesCodec{}), grpc.UnknownServiceHandler(func(_ interface{}, stream grpc.ServerStream) error {
		var in []byte
		if err := stream.RecvMsg(&in); err != nil {
			return err
		}
		out := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x03} // not valid protobuf
		return stream.SendMsg(&out)
	}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	sched, healthStore := testHealthCheckScheduler(t)
	runGRPCHealthCheck(t, sched, lis.Addr().String())
	if !healthStore.IsHealthy("testnet", "test-node", "grpc") {
		t.Errorf("node answering non-protobuf bytes marked unhealthy: %+v", healthStore.GetHealth("testnet", "test-node", "grpc"))
	}
}

// delayedProxy forwards TCP to target, holding the first bytes from the
// server for delay: a node whose connection takes that long to become READY.
func delayedProxy(t *testing.T, target string, delay time.Duration) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				b, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer func() { _ = b.Close() }()
				go func() { _, _ = io.Copy(b, c) }()
				time.Sleep(delay)
				_, _ = io.Copy(c, b)
			}(c)
		}
	}()
	return lis.Addr().String()
}

// The query gets the whole check timeout of its own: a connection that is
// slow to become READY must not leave the query too little time and mark a
// node that answers as unhealthy.
func TestScheduleGRPCHealthCheck_QueryHasItsOwnTimeout(t *testing.T) {
	t.Parallel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ interface{}, stream grpc.ServerStream) error {
		var req emptypb.Empty
		if err := stream.RecvMsg(&req); err != nil {
			return err
		}
		time.Sleep(200 * time.Millisecond) // a query that takes a while
		return stream.SendMsg(&emptypb.Empty{})
	}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	addr := delayedProxy(t, lis.Addr().String(), 400*time.Millisecond)

	sched, healthStore := testHealthCheckScheduler(t)
	sched.timeout.Store(int64(500 * time.Millisecond))
	runGRPCHealthCheck(t, sched, addr)
	if !healthStore.IsHealthy("testnet", "test-node", "grpc") {
		t.Errorf("node READY after 400ms and answering in 200ms marked unhealthy with a 500ms timeout: %+v", healthStore.GetHealth("testnet", "test-node", "grpc"))
	}
}
