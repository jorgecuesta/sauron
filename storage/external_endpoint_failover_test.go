package storage

import (
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"sauron/metrics"
)

func recoveriesCount(network, epType, external string) float64 {
	var m dto.Metric
	_ = metrics.ExternalEndpointRecoveries.WithLabelValues(network, epType, external).Write(&m)
	return m.GetCounter().GetValue()
}

func errorCountGauge(network, epType, url string) float64 {
	var m dto.Metric
	_ = metrics.ExternalEndpointErrorCount.WithLabelValues(network, epType, url).Write(&m)
	return m.GetGauge().GetValue()
}

// findEndpoint returns the stored endpoint for url (a copy), or nil.
func findEndpoint(s *ExternalEndpointStore, externalName, ringURL, network string, url string) *ExternalEndpoint {
	for _, ep := range s.EndpointsForRing(externalName, ringURL, network) {
		if ep.URL == url {
			return ep
		}
	}
	return nil
}

// #17: an endpoint that was validated and then fails validation is FAILED,
// not "never validated": it is listed for recovery, its error count shows,
// and validating it again is a recovery.
func TestExternalEndpoints_ValidationFailureOfValidatedEndpointIsRecoverable(t *testing.T) {
	t.Parallel()
	s := newTestEndpointStore()
	const ext, ring, net, typ, url = "ext-vf", "https://ring-vf.example.com", "pocket-vf", "rest", "https://ep-vf.example.com"

	s.StoreAdvertised(ext, ring, net, typ, url)
	s.MarkValidated(ext, ring, net, typ, url, 100, time.Millisecond)
	recBefore := recoveriesCount(net, typ, ext)

	s.MarkValidationFailed(ext, ring, net, typ, url)
	s.MarkValidationFailed(ext, ring, net, typ, url)

	var got *ExternalEndpoint
	for _, ep := range s.GetFailedEndpoints() {
		if ep.URL == url {
			got = ep
		}
	}
	if got == nil {
		t.Fatal("validated endpoint that failed validation is not listed for recovery")
	}
	if !got.IsValidated || got.IsWorking {
		t.Errorf("state = validated %v working %v, want validated true working false", got.IsValidated, got.IsWorking)
	}
	if got.ErrorCount != 2 {
		t.Errorf("ErrorCount = %d, want 2 after two failed validations", got.ErrorCount)
	}
	if got.Height != 100 {
		t.Errorf("Height = %d, want the last known 100", got.Height)
	}
	if g := errorCountGauge(net, typ, url); g != 2 {
		t.Errorf("error count gauge = %v, want 2", g)
	}
	if len(s.GetValidatedEndpoints(net, typ)) != 0 {
		t.Error("failed endpoint still routable")
	}

	s.MarkValidated(ext, ring, net, typ, url, 120, time.Millisecond)
	if d := recoveriesCount(net, typ, ext) - recBefore; d != 1 {
		t.Errorf("recoveries +%v, want +1 for FAILED -> validated", d)
	}
	ep := findEndpoint(s, ext, ring, net, url)
	if ep == nil || !ep.IsWorking || ep.ErrorCount != 0 || ep.Height != 120 {
		t.Errorf("after recovery: %+v, want working, ErrorCount 0, height 120", ep)
	}
	if g := errorCountGauge(net, typ, url); g != 0 {
		t.Errorf("error count gauge = %v after recovery, want 0", g)
	}

	// Validating a working endpoint again is not a recovery.
	s.MarkValidated(ext, ring, net, typ, url, 121, time.Millisecond)
	if d := recoveriesCount(net, typ, ext) - recBefore; d != 1 {
		t.Errorf("recoveries +%v after a plain revalidation, want still +1", d)
	}
}

// An endpoint that never passed validation stays unvalidated on failure: it
// is not listed for recovery, and its first validation is not a recovery.
func TestExternalEndpoints_NeverValidatedFailureIsNotRecovery(t *testing.T) {
	t.Parallel()
	s := newTestEndpointStore()
	const ext, ring, net, typ, url = "ext-nv", "https://ring-nv.example.com", "pocket-nv", "rest", "https://ep-nv.example.com"

	s.StoreAdvertised(ext, ring, net, typ, url)
	s.MarkValidationFailed(ext, ring, net, typ, url)
	for _, ep := range s.GetFailedEndpoints() {
		if ep.URL == url {
			t.Fatal("never-validated endpoint listed for recovery")
		}
	}
	before := recoveriesCount(net, typ, ext)
	s.MarkValidated(ext, ring, net, typ, url, 100, time.Millisecond)
	if d := recoveriesCount(net, typ, ext) - before; d != 0 {
		t.Errorf("first validation counted as recovery (+%v)", d)
	}
}

func TestExternalEndpoints_MarkValidationFailedUnknownEndpoint(t *testing.T) {
	t.Parallel()
	s := newTestEndpointStore()
	s.MarkValidationFailed("nobody", "https://ring.example.com", "pocket", "rest", "https://ghost.example.com")
	if len(s.GetFailedEndpoints()) != 0 {
		t.Error("unknown endpoint created by MarkValidationFailed")
	}
}

func TestExternalEndpoints_EndpointsForRing(t *testing.T) {
	t.Parallel()
	s := newTestEndpointStore()
	s.StoreAdvertised("ext-a", "https://ring-a", "pocket", "rest", "https://a-rest")
	s.StoreAdvertised("ext-a", "https://ring-a", "pocket", "rpc", "https://a-rpc")
	s.StoreAdvertised("ext-a", "https://ring-a", "beta", "rest", "https://a-beta")
	s.StoreAdvertised("ext-a", "https://ring-b", "pocket", "rest", "https://b-rest")
	s.StoreAdvertised("ext-b", "https://ring-a", "pocket", "rest", "https://other-ext")

	got := map[string]bool{}
	for _, ep := range s.EndpointsForRing("ext-a", "https://ring-a", "pocket") {
		got[ep.URL] = true
	}
	if len(got) != 2 || !got["https://a-rest"] || !got["https://a-rpc"] {
		t.Errorf("EndpointsForRing = %v, want a-rest and a-rpc only", got)
	}
	if eps := s.EndpointsForRing("nobody", "https://ring-a", "pocket"); len(eps) != 0 {
		t.Errorf("unknown external returned %d endpoints", len(eps))
	}

	// Copies: mutating the result does not touch the store.
	eps := s.EndpointsForRing("ext-a", "https://ring-a", "pocket")
	eps[0].IsWorking = true
	eps[0].URL = "mutated"
	for _, ep := range s.EndpointsForRing("ext-a", "https://ring-a", "pocket") {
		if ep.URL == "mutated" || ep.IsWorking {
			t.Errorf("store mutated through returned copy: %+v", ep)
		}
	}
}
