package storage

import (
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func newTestEndpointStore() *ExternalEndpointStore {
	return NewExternalEndpointStore(zap.NewNop())
}

func TestExternalEndpoints_StoreAndValidate(t *testing.T) {
	t.Parallel()
	s := newTestEndpointStore()

	s.StoreAdvertised("ext-1", "https://ring1.example.com", "pocket", "rest", "https://ep1.example.com")
	s.MarkValidated("ext-1", "https://ring1.example.com", "pocket", "rest", "https://ep1.example.com", 500, 20*time.Millisecond)

	eps := s.GetValidatedEndpoints("pocket", "rest")
	if len(eps) != 1 {
		t.Fatalf("expected 1 validated endpoint, got %d", len(eps))
	}

	ep := eps[0]
	if ep.URL != "https://ep1.example.com" {
		t.Errorf("expected URL https://ep1.example.com, got %s", ep.URL)
	}
	if ep.Height != 500 {
		t.Errorf("expected height 500, got %d", ep.Height)
	}
	if ep.Latency != 20*time.Millisecond {
		t.Errorf("expected latency 20ms, got %v", ep.Latency)
	}
	if !ep.IsValidated {
		t.Error("expected IsValidated=true")
	}
	if !ep.IsWorking {
		t.Error("expected IsWorking=true")
	}
}

func TestExternalEndpoints_ValidationFailed(t *testing.T) {
	t.Parallel()
	s := newTestEndpointStore()

	s.StoreAdvertised("ext-1", "https://ring1.example.com", "pocket", "rest", "https://ep1.example.com")
	s.MarkValidated("ext-1", "https://ring1.example.com", "pocket", "rest", "https://ep1.example.com", 100, 10*time.Millisecond)
	s.MarkValidationFailed("ext-1", "https://ring1.example.com", "pocket", "rest", "https://ep1.example.com")

	eps := s.GetValidatedEndpoints("pocket", "rest")
	if len(eps) != 0 {
		t.Errorf("expected 0 validated endpoints after failure, got %d", len(eps))
	}
}

func TestExternalEndpoints_TrackProxyError_ThresholdAt3(t *testing.T) {
	t.Parallel()
	s := newTestEndpointStore()

	s.StoreAdvertised("ext-1", "https://ring1.example.com", "pocket", "rest", "https://ep1.example.com")
	s.MarkValidated("ext-1", "https://ring1.example.com", "pocket", "rest", "https://ep1.example.com", 100, 10*time.Millisecond)

	// 2 errors: should still be working
	s.TrackProxyError("pocket", "rest", "https://ep1.example.com")
	s.TrackProxyError("pocket", "rest", "https://ep1.example.com")

	eps := s.GetValidatedEndpoints("pocket", "rest")
	if len(eps) != 1 {
		t.Fatalf("expected 1 working endpoint after 2 errors, got %d", len(eps))
	}

	// 3rd error: should be marked not working
	s.TrackProxyError("pocket", "rest", "https://ep1.example.com")

	eps = s.GetValidatedEndpoints("pocket", "rest")
	if len(eps) != 0 {
		t.Errorf("expected 0 working endpoints after 3 errors (threshold), got %d", len(eps))
	}
}

func TestExternalEndpoints_RecoverFailed(t *testing.T) {
	t.Parallel()
	s := newTestEndpointStore()

	s.StoreAdvertised("ext-1", "https://ring1.example.com", "pocket", "rest", "https://ep1.example.com")
	// Validate then mark as failed via 3 proxy errors
	s.MarkValidated("ext-1", "https://ring1.example.com", "pocket", "rest", "https://ep1.example.com", 100, 10*time.Millisecond)
	s.TrackProxyError("pocket", "rest", "https://ep1.example.com")
	s.TrackProxyError("pocket", "rest", "https://ep1.example.com")
	s.TrackProxyError("pocket", "rest", "https://ep1.example.com")

	failed := s.GetFailedEndpoints()
	if len(failed) != 1 {
		t.Fatalf("expected 1 failed endpoint, got %d", len(failed))
	}

	ep := failed[0]
	if ep.URL != "https://ep1.example.com" {
		t.Errorf("unexpected URL in failed list: %s", ep.URL)
	}
	if ep.IsWorking {
		t.Error("expected IsWorking=false for failed endpoint")
	}
	if !ep.IsValidated {
		t.Error("expected IsValidated=true (was validated, then failed during proxying)")
	}
}

func TestExternalEndpoints_FiltersNetwork(t *testing.T) {
	t.Parallel()
	s := newTestEndpointStore()

	// Two endpoints in different networks/types
	s.StoreAdvertised("ext-1", "https://ring1.example.com", "pocket", "rest", "https://pocket-ep.example.com")
	s.MarkValidated("ext-1", "https://ring1.example.com", "pocket", "rest", "https://pocket-ep.example.com", 100, 10*time.Millisecond)

	s.StoreAdvertised("ext-2", "https://ring2.example.com", "pocket-beta", "rest", "https://beta-ep.example.com")
	s.MarkValidated("ext-2", "https://ring2.example.com", "pocket-beta", "rest", "https://beta-ep.example.com", 200, 10*time.Millisecond)

	s.StoreAdvertised("ext-3", "https://ring3.example.com", "pocket", "rpc", "https://pocket-rpc.example.com")
	s.MarkValidated("ext-3", "https://ring3.example.com", "pocket", "rpc", "https://pocket-rpc.example.com", 300, 10*time.Millisecond)

	pocketAPI := s.GetValidatedEndpoints("pocket", "rest")
	if len(pocketAPI) != 1 {
		t.Errorf("expected 1 pocket/api endpoint, got %d", len(pocketAPI))
	}
	if len(pocketAPI) > 0 && pocketAPI[0].URL != "https://pocket-ep.example.com" {
		t.Errorf("unexpected URL in pocket/api: %s", pocketAPI[0].URL)
	}

	betaAPI := s.GetValidatedEndpoints("pocket-beta", "rest")
	if len(betaAPI) != 1 {
		t.Errorf("expected 1 pocket-beta/api endpoint, got %d", len(betaAPI))
	}

	pocketRPC := s.GetValidatedEndpoints("pocket", "rpc")
	if len(pocketRPC) != 1 {
		t.Errorf("expected 1 pocket/rpc endpoint, got %d", len(pocketRPC))
	}
}

// TestUpdateWebSocketAvailability_SetsTrue verifies that UpdateWebSocketAvailability
// sets WebSocketAvailable=true on an existing endpoint.
func TestUpdateWebSocketAvailability_SetsTrue(t *testing.T) {
	t.Parallel()
	s := newTestEndpointStore()

	s.StoreAdvertised("ext-1", "https://ring.example.com", "pocket", "rpc", "https://rpc.example.com")
	s.MarkValidated("ext-1", "https://ring.example.com", "pocket", "rpc", "https://rpc.example.com", 1000, 5*time.Millisecond)

	s.UpdateWebSocketAvailability("ext-1", "https://ring.example.com", "pocket", "rpc", "https://rpc.example.com", true)

	eps := s.GetValidatedEndpoints("pocket", "rpc")
	if len(eps) != 1 {
		t.Fatalf("expected 1 validated rpc endpoint, got %d", len(eps))
	}
	if !eps[0].WebSocketAvailable {
		t.Error("expected WebSocketAvailable=true after UpdateWebSocketAvailability(true)")
	}
}

// TestUpdateWebSocketAvailability_SetsFalse verifies that UpdateWebSocketAvailability
// sets WebSocketAvailable=false on an existing endpoint.
func TestUpdateWebSocketAvailability_SetsFalse(t *testing.T) {
	t.Parallel()
	s := newTestEndpointStore()

	s.StoreAdvertised("ext-1", "https://ring.example.com", "pocket", "rpc", "https://rpc.example.com")
	s.MarkValidated("ext-1", "https://ring.example.com", "pocket", "rpc", "https://rpc.example.com", 1000, 5*time.Millisecond)

	// Set true first, then set false.
	s.UpdateWebSocketAvailability("ext-1", "https://ring.example.com", "pocket", "rpc", "https://rpc.example.com", true)
	s.UpdateWebSocketAvailability("ext-1", "https://ring.example.com", "pocket", "rpc", "https://rpc.example.com", false)

	eps := s.GetValidatedEndpoints("pocket", "rpc")
	if len(eps) != 1 {
		t.Fatalf("expected 1 validated rpc endpoint, got %d", len(eps))
	}
	if eps[0].WebSocketAvailable {
		t.Error("expected WebSocketAvailable=false after UpdateWebSocketAvailability(false)")
	}
}

// TestUpdateWebSocketAvailability_NonExistentEndpoint verifies that
// UpdateWebSocketAvailability is a no-op when the endpoint does not exist
// (should not panic or create a new entry).
func TestUpdateWebSocketAvailability_NonExistentEndpoint(t *testing.T) {
	t.Parallel()
	s := newTestEndpointStore()

	// Call on an endpoint that was never stored — must not panic.
	s.UpdateWebSocketAvailability("nonexistent", "https://ring.example.com", "pocket", "rpc", "https://unknown.example.com", true)

	// No entries should have been created.
	eps := s.GetValidatedEndpoints("pocket", "rpc")
	if len(eps) != 0 {
		t.Errorf("expected 0 endpoints after no-op call, got %d", len(eps))
	}
}

func TestExternalEndpoints_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	s := newTestEndpointStore()

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Writers
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			url := "https://ep" + string(rune('A'+i%26)) + ".example.com"
			s.StoreAdvertised("ext", "https://ring.example.com", "pocket", "rest", url)
			s.MarkValidated("ext", "https://ring.example.com", "pocket", "rest", url, int64(i*10), time.Duration(i)*time.Millisecond)
		}()
	}

	// Readers
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = s.GetValidatedEndpoints("pocket", "rest")
			_ = s.GetFailedEndpoints()
		}()
	}

	wg.Wait()
}

func TestExternalEndpointStore_GRPCTransport(t *testing.T) {
	t.Parallel()

	s := NewExternalEndpointStore(zap.NewNop())
	if _, known := s.GRPCTransport("grpc.example.com:443"); known {
		t.Fatal("unknown URL: want known=false")
	}

	s.StoreAdvertised("ring-a", "https://ring-a:3000", "pocket", "grpc", "grpc.example.com:443")
	if _, known := s.GRPCTransport("grpc.example.com:443"); known {
		t.Fatal("advertised but never validated: want known=false")
	}

	s.SetGRPCTransport("ring-a", "https://ring-a:3000", "pocket", "grpc", "grpc.example.com:443", false)
	if insecure, known := s.GRPCTransport("grpc.example.com:443"); !known || insecure {
		t.Fatalf("after TLS validation: got insecure=%v known=%v", insecure, known)
	}
	s.SetGRPCTransport("ring-a", "https://ring-a:3000", "pocket", "grpc", "grpc.example.com:443", true)
	if insecure, known := s.GRPCTransport("grpc.example.com:443"); !known || !insecure {
		t.Fatalf("after plaintext validation: got insecure=%v known=%v", insecure, known)
	}

	// Setting it on an endpoint the store does not hold changes nothing.
	s.SetGRPCTransport("ring-b", "https://ring-b:3000", "pocket", "grpc", "other:9090", true)
	if _, known := s.GRPCTransport("other:9090"); known {
		t.Fatal("non-existent endpoint: want known=false")
	}
}

// Concurrent writes and reads of the transport, for the race detector.
func TestExternalEndpointStore_GRPCTransportConcurrent(t *testing.T) {
	t.Parallel()

	s := NewExternalEndpointStore(zap.NewNop())
	s.StoreAdvertised("ring-a", "https://ring-a:3000", "pocket", "grpc", "g:443")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.SetGRPCTransport("ring-a", "https://ring-a:3000", "pocket", "grpc", "g:443", (i+j)%2 == 0)
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.GRPCTransport("g:443")
			}
		}()
	}
	wg.Wait()
	if _, known := s.GRPCTransport("g:443"); !known {
		t.Fatal("want known=true after concurrent writes")
	}
}

// Two rings advertising one URL share its recorded transport, the last
// validation winning; removing one ring keeps it, removing both forgets it.
func TestExternalEndpointStore_GRPCTransportSharedURL(t *testing.T) {
	t.Parallel()

	s := NewExternalEndpointStore(zap.NewNop())
	s.StoreAdvertised("ring-a", "https://ring-a:3000", "pocket", "grpc", "g:443")
	s.StoreAdvertised("ring-b", "https://ring-b:3000", "pocket", "grpc", "g:443")
	s.SetGRPCTransport("ring-a", "https://ring-a:3000", "pocket", "grpc", "g:443", true)
	s.SetGRPCTransport("ring-b", "https://ring-b:3000", "pocket", "grpc", "g:443", false)
	for i := 0; i < 20; i++ {
		if insecure, known := s.GRPCTransport("g:443"); !known || insecure {
			t.Fatalf("got insecure=%v known=%v, want the last validation (TLS)", insecure, known)
		}
	}

	s.RemoveExternals(func(name, _ string) bool { return name != "ring-a" })
	if _, known := s.GRPCTransport("g:443"); !known {
		t.Fatal("ring-b still advertises g:443: transport must be kept")
	}
	s.RemoveExternals(func(string, string) bool { return false })
	if _, known := s.GRPCTransport("g:443"); known {
		t.Fatal("no ring advertises g:443: transport must be forgotten")
	}
}
