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

	s.StoreAdvertised("ext-1", "https://ring1.example.com", "pocket", "api", "https://ep1.example.com")
	s.MarkValidated("ext-1", "https://ring1.example.com", "pocket", "api", "https://ep1.example.com", 500, 20*time.Millisecond)

	eps := s.GetValidatedEndpoints("pocket", "api")
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

	s.StoreAdvertised("ext-1", "https://ring1.example.com", "pocket", "api", "https://ep1.example.com")
	s.MarkValidated("ext-1", "https://ring1.example.com", "pocket", "api", "https://ep1.example.com", 100, 10*time.Millisecond)
	s.MarkValidationFailed("ext-1", "https://ring1.example.com", "pocket", "api", "https://ep1.example.com")

	eps := s.GetValidatedEndpoints("pocket", "api")
	if len(eps) != 0 {
		t.Errorf("expected 0 validated endpoints after failure, got %d", len(eps))
	}
}

func TestExternalEndpoints_TrackProxyError_ThresholdAt3(t *testing.T) {
	t.Parallel()
	s := newTestEndpointStore()

	s.StoreAdvertised("ext-1", "https://ring1.example.com", "pocket", "api", "https://ep1.example.com")
	s.MarkValidated("ext-1", "https://ring1.example.com", "pocket", "api", "https://ep1.example.com", 100, 10*time.Millisecond)

	// 2 errors: should still be working
	s.TrackProxyError("pocket", "api", "https://ep1.example.com")
	s.TrackProxyError("pocket", "api", "https://ep1.example.com")

	eps := s.GetValidatedEndpoints("pocket", "api")
	if len(eps) != 1 {
		t.Fatalf("expected 1 working endpoint after 2 errors, got %d", len(eps))
	}

	// 3rd error: should be marked not working
	s.TrackProxyError("pocket", "api", "https://ep1.example.com")

	eps = s.GetValidatedEndpoints("pocket", "api")
	if len(eps) != 0 {
		t.Errorf("expected 0 working endpoints after 3 errors (threshold), got %d", len(eps))
	}
}

func TestExternalEndpoints_RecoverFailed(t *testing.T) {
	t.Parallel()
	s := newTestEndpointStore()

	s.StoreAdvertised("ext-1", "https://ring1.example.com", "pocket", "api", "https://ep1.example.com")
	// Validate then mark as failed via 3 proxy errors
	s.MarkValidated("ext-1", "https://ring1.example.com", "pocket", "api", "https://ep1.example.com", 100, 10*time.Millisecond)
	s.TrackProxyError("pocket", "api", "https://ep1.example.com")
	s.TrackProxyError("pocket", "api", "https://ep1.example.com")
	s.TrackProxyError("pocket", "api", "https://ep1.example.com")

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
	s.StoreAdvertised("ext-1", "https://ring1.example.com", "pocket", "api", "https://pocket-ep.example.com")
	s.MarkValidated("ext-1", "https://ring1.example.com", "pocket", "api", "https://pocket-ep.example.com", 100, 10*time.Millisecond)

	s.StoreAdvertised("ext-2", "https://ring2.example.com", "pocket-beta", "api", "https://beta-ep.example.com")
	s.MarkValidated("ext-2", "https://ring2.example.com", "pocket-beta", "api", "https://beta-ep.example.com", 200, 10*time.Millisecond)

	s.StoreAdvertised("ext-3", "https://ring3.example.com", "pocket", "rpc", "https://pocket-rpc.example.com")
	s.MarkValidated("ext-3", "https://ring3.example.com", "pocket", "rpc", "https://pocket-rpc.example.com", 300, 10*time.Millisecond)

	pocketAPI := s.GetValidatedEndpoints("pocket", "api")
	if len(pocketAPI) != 1 {
		t.Errorf("expected 1 pocket/api endpoint, got %d", len(pocketAPI))
	}
	if len(pocketAPI) > 0 && pocketAPI[0].URL != "https://pocket-ep.example.com" {
		t.Errorf("unexpected URL in pocket/api: %s", pocketAPI[0].URL)
	}

	betaAPI := s.GetValidatedEndpoints("pocket-beta", "api")
	if len(betaAPI) != 1 {
		t.Errorf("expected 1 pocket-beta/api endpoint, got %d", len(betaAPI))
	}

	pocketRPC := s.GetValidatedEndpoints("pocket", "rpc")
	if len(pocketRPC) != 1 {
		t.Errorf("expected 1 pocket/rpc endpoint, got %d", len(pocketRPC))
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
			s.StoreAdvertised("ext", "https://ring.example.com", "pocket", "api", url)
			s.MarkValidated("ext", "https://ring.example.com", "pocket", "api", url, int64(i*10), time.Duration(i)*time.Millisecond)
		}()
	}

	// Readers
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = s.GetValidatedEndpoints("pocket", "api")
			_ = s.GetFailedEndpoints()
		}()
	}

	wg.Wait()
}
