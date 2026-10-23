package checker

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tmservice "cosmossdk.io/api/cosmos/base/tendermint/v1beta1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"sauron/config"
	"sauron/internal/grpcutil"
)

// Answers of fakeTMService.GetLatestBlock.
const (
	tmBlock       int32 = iota // a block with a header
	tmNoBlock                  // a response without sdk_block
	tmNoHeader                 // sdk_block without header
	tmUnavailable              // status Unavailable
)

// fakeTMService is a Cosmos Tendermint query service, as an external Sauron
// ring advertises for gRPC.
type fakeTMService struct {
	tmservice.UnimplementedServiceServer
	mode  atomic.Int32
	calls atomic.Int32
}

func (f *fakeTMService) GetLatestBlock(context.Context, *tmservice.GetLatestBlockRequest) (*tmservice.GetLatestBlockResponse, error) {
	f.calls.Add(1)
	switch f.mode.Load() {
	case tmNoBlock:
		return &tmservice.GetLatestBlockResponse{}, nil
	case tmNoHeader:
		return &tmservice.GetLatestBlockResponse{SdkBlock: &tmservice.Block{}}, nil
	case tmUnavailable:
		return nil, status.Error(codes.Unavailable, "node down")
	}
	return &tmservice.GetLatestBlockResponse{SdkBlock: &tmservice.Block{Header: &tmservice.Header{Height: 976400, ChainId: "pocket"}}}, nil
}

func startTMService(t *testing.T) (*fakeTMService, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeTMService{}
	srv := grpc.NewServer()
	tmservice.RegisterServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return f, lis.Addr().String()
}

// startRing serves an external ring's /{network}/status at height 100.
func startRing(t *testing.T, endpoints map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(newStatusHandler(100, endpoints))
	t.Cleanup(srv.Close)
	return srv.URL
}

// An external gRPC endpoint is valid only when GetLatestBlock returns a block
// with a header; an error or an empty answer is not.
func TestValidateGRPCEndpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mode    int32
		wantErr string
	}{
		{name: "block with header", mode: tmBlock},
		{name: "no sdk_block", mode: tmNoBlock, wantErr: "nil block or header"},
		{name: "no header", mode: tmNoHeader, wantErr: "nil block or header"},
		{name: "node error", mode: tmUnavailable, wantErr: "gRPC call failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc, addr := startTMService(t)
			svc.mode.Store(tt.mode)
			checker, _, _ := newTestExternalChecker(t, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			latency, err := checker.validateGRPCEndpoint(ctx, addr, true)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if latency <= 0 {
					t.Errorf("latency = %v, want measured", latency)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
			if tt.mode == tmUnavailable && status.Code(err) != codes.Unavailable {
				t.Errorf("status code through the wrap = %s, want Unavailable", status.Code(err))
			}
		})
	}
}

// One connection per endpoint URL and transport is created and reused across validations;
// the warmup call on creation also reaches the node.
func TestValidateGRPCEndpoint_ReusesConnection(t *testing.T) {
	t.Parallel()
	svc, addr := startTMService(t)
	checker, _, _ := newTestExternalChecker(t, nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := checker.validateGRPCEndpoint(ctx, addr, true); err != nil {
			t.Fatalf("validation %d: %v", i, err)
		}
	}
	first, ok := checker.grpcConnections.Load(grpcutil.ConnKey(addr, true))
	if !ok {
		t.Fatal("no cached connection")
	}
	n := 0
	checker.grpcConnections.Range(func(string, *grpc.ClientConn) bool { n++; return true })
	if n != 1 {
		t.Errorf("cached connections = %d, want 1", n)
	}
	if again, _ := checker.grpcConnections.Load(grpcutil.ConnKey(addr, true)); again != first {
		t.Error("cached connection replaced")
	}
	if got := svc.calls.Load(); got != 4 {
		t.Errorf("GetLatestBlock calls = %d, want 4 (warmup + 3 validations)", got)
	}
}

// A connection whose warmup fails (node not answering yet) is still cached,
// and validation succeeds once the node answers.
func TestValidateGRPCEndpoint_WarmupFailureKeepsConnection(t *testing.T) {
	t.Parallel()
	svc, addr := startTMService(t)
	svc.mode.Store(tmUnavailable)
	checker, _, _ := newTestExternalChecker(t, nil)
	ctx := context.Background()
	if _, err := checker.validateGRPCEndpoint(ctx, addr, true); err == nil {
		t.Fatal("validation succeeded against a failing node")
	}
	if _, ok := checker.grpcConnections.Load(grpcutil.ConnKey(addr, true)); !ok {
		t.Fatal("connection not cached after a failed warmup")
	}
	svc.mode.Store(tmBlock)
	if _, err := checker.validateGRPCEndpoint(ctx, addr, true); err != nil {
		t.Errorf("validation after the node answers: %v", err)
	}
}

// A ring advertising a gRPC endpoint gets it validated through
// GetLatestBlock; a failed gRPC endpoint recovers through it too.
func TestExternalGRPCEndpoint_ValidatedAndRecovered(t *testing.T) {
	t.Parallel()
	svc, addr := startTMService(t)
	ring := startRing(t, map[string]string{"grpc": addr})
	checker, _, endpointStore := newTestExternalChecker(t, nil)
	ext := config.External{Name: "grpc-ext", Rings: []string{ring}}

	if err := checker.queryRing(context.Background(), ext, ring, "pocket"); err != nil {
		t.Fatalf("queryRing: %v", err)
	}
	if eps := endpointStore.GetValidatedEndpoints("pocket", "grpc"); len(eps) != 1 || eps[0].URL != addr || eps[0].Height != 100 {
		t.Fatalf("validated grpc endpoints = %+v, want %s at height 100", eps, addr)
	}

	svc.mode.Store(tmUnavailable)
	checker.revalidateRingEndpoints(context.Background(), ext.Name, ring, "pocket")
	if len(endpointStore.GetValidatedEndpoints("pocket", "grpc")) != 0 {
		t.Fatal("failing grpc endpoint still routable")
	}

	svc.mode.Store(tmBlock)
	checker.RecoverFailedEndpoints(context.Background())
	if eps := endpointStore.GetValidatedEndpoints("pocket", "grpc"); len(eps) != 1 {
		t.Errorf("recovered grpc endpoints = %d, want 1", len(eps))
	}
}
