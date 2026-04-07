package checker

import (
	"net/http"
	"testing"
)

// TestNewCheckerHTTPClient verifies that the transport config matches the
// constants defined in constants.go.
func TestNewCheckerHTTPClient(t *testing.T) {
	client := newCheckerHTTPClient()
	if client == nil {
		t.Fatal("newCheckerHTTPClient returned nil")
	}

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", client.Transport)
	}

	if transport.MaxIdleConns != HTTPMaxIdleConns {
		t.Errorf("MaxIdleConns: got %d, want %d", transport.MaxIdleConns, HTTPMaxIdleConns)
	}
	if transport.MaxIdleConnsPerHost != HTTPMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost: got %d, want %d", transport.MaxIdleConnsPerHost, HTTPMaxIdleConnsPerHost)
	}
	if transport.MaxConnsPerHost != HTTPMaxConnsPerHost {
		t.Errorf("MaxConnsPerHost: got %d, want %d", transport.MaxConnsPerHost, HTTPMaxConnsPerHost)
	}
	if transport.IdleConnTimeout != HTTPIdleConnTimeout {
		t.Errorf("IdleConnTimeout: got %v, want %v", transport.IdleConnTimeout, HTTPIdleConnTimeout)
	}
}
