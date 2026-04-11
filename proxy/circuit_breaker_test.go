package proxy

import (
	"sync"
	"testing"

	"sauron/storage"

	"go.uber.org/zap"
)

func TestCircuitBreaker_RecordError_IncrementsCount(t *testing.T) {
	t.Parallel()
	hs := storage.NewHealthStore()
	hs.SetHealthy("net", "node-1", "rpc")
	cb := NewCircuitBreaker(hs, zap.NewNop())

	cb.RecordError("net", "node-1", "rpc", 3)
	if cb.GetErrorCount("net", "node-1", "rpc") != 1 {
		t.Fatalf("expected 1 error, got %d", cb.GetErrorCount("net", "node-1", "rpc"))
	}

	cb.RecordError("net", "node-1", "rpc", 3)
	if cb.GetErrorCount("net", "node-1", "rpc") != 2 {
		t.Fatalf("expected 2 errors, got %d", cb.GetErrorCount("net", "node-1", "rpc"))
	}

	// Still healthy before threshold
	if !hs.IsHealthy("net", "node-1", "rpc") {
		t.Fatal("node should still be healthy before threshold")
	}
}

func TestCircuitBreaker_TripsAtThreshold(t *testing.T) {
	t.Parallel()
	hs := storage.NewHealthStore()
	hs.SetHealthy("net", "node-1", "jsonrpc")
	cb := NewCircuitBreaker(hs, zap.NewNop())

	// 3 errors with threshold 3 → should trip
	cb.RecordError("net", "node-1", "jsonrpc", 3)
	cb.RecordError("net", "node-1", "jsonrpc", 3)
	cb.RecordError("net", "node-1", "jsonrpc", 3)

	if hs.IsHealthy("net", "node-1", "jsonrpc") {
		t.Fatal("node should be unhealthy after 3 errors (threshold=3)")
	}

	h := hs.GetHealth("net", "node-1", "jsonrpc")
	if h == nil || h.LastError == "" {
		t.Fatal("expected LastError to be set with circuit breaker reason")
	}
}

func TestCircuitBreaker_RecordSuccess_ResetsCount(t *testing.T) {
	t.Parallel()
	hs := storage.NewHealthStore()
	hs.SetHealthy("net", "node-1", "rpc")
	cb := NewCircuitBreaker(hs, zap.NewNop())

	cb.RecordError("net", "node-1", "rpc", 5)
	cb.RecordError("net", "node-1", "rpc", 5)
	if cb.GetErrorCount("net", "node-1", "rpc") != 2 {
		t.Fatalf("expected 2, got %d", cb.GetErrorCount("net", "node-1", "rpc"))
	}

	cb.RecordSuccess("net", "node-1", "rpc")
	if cb.GetErrorCount("net", "node-1", "rpc") != 0 {
		t.Fatalf("expected 0 after success, got %d", cb.GetErrorCount("net", "node-1", "rpc"))
	}
}

func TestCircuitBreaker_NilSafe(t *testing.T) {
	t.Parallel()
	var cb *CircuitBreaker

	// Should not panic
	cb.RecordError("net", "node-1", "rpc", 3)
	cb.RecordSuccess("net", "node-1", "rpc")
	if cb.GetErrorCount("net", "node-1", "rpc") != 0 {
		t.Fatal("nil circuit breaker should return 0")
	}
}

func TestCircuitBreaker_PerNodeIsolation(t *testing.T) {
	t.Parallel()
	hs := storage.NewHealthStore()
	hs.SetHealthy("net", "node-1", "rpc")
	hs.SetHealthy("net", "node-2", "rpc")
	cb := NewCircuitBreaker(hs, zap.NewNop())

	// Trip node-1
	for i := 0; i < 3; i++ {
		cb.RecordError("net", "node-1", "rpc", 3)
	}

	// node-1 unhealthy, node-2 still healthy
	if hs.IsHealthy("net", "node-1", "rpc") {
		t.Fatal("node-1 should be unhealthy")
	}
	if !hs.IsHealthy("net", "node-2", "rpc") {
		t.Fatal("node-2 should still be healthy")
	}
}

func TestCircuitBreaker_Concurrent(t *testing.T) {
	t.Parallel()
	hs := storage.NewHealthStore()
	hs.SetHealthy("net", "node-1", "rpc")
	cb := NewCircuitBreaker(hs, zap.NewNop())

	var wg sync.WaitGroup
	wg.Add(20)
	for i := 0; i < 10; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				cb.RecordError("net", "node-1", "rpc", 1000)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				cb.RecordSuccess("net", "node-1", "rpc")
			}
		}()
	}
	wg.Wait()
	// No race, no panic — that's the test
}

func TestCircuitBreaker_ZeroThreshold_NoOp(t *testing.T) {
	t.Parallel()
	hs := storage.NewHealthStore()
	hs.SetHealthy("net", "node-1", "rpc")
	cb := NewCircuitBreaker(hs, zap.NewNop())

	cb.RecordError("net", "node-1", "rpc", 0) // threshold 0 = disabled
	if !hs.IsHealthy("net", "node-1", "rpc") {
		t.Fatal("threshold 0 should not trip circuit breaker")
	}
}

func TestIsRetriableHTTPCode(t *testing.T) {
	t.Parallel()
	codes := []int{429, 500, 502, 503}

	tests := []struct {
		code int
		want bool
	}{
		{200, false},
		{400, false},
		{404, false},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{504, false},
	}

	for _, tt := range tests {
		got := isRetriableHTTPCode(tt.code, codes)
		if got != tt.want {
			t.Errorf("isRetriableHTTPCode(%d) = %v, want %v", tt.code, got, tt.want)
		}
	}
}
