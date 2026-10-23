package checker

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"testing"
	"time"

	"sauron/internal/testtls"
)

// testServerTLS presents a certificate the process trusts (SSL_CERT_FILE is
// set before any test runs).
var testServerTLS *tls.Config

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sauron-checker-tls-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if testServerTLS, err = testtls.Install(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// An external ring may advertise a plaintext or a TLS gRPC endpoint; the
// checker must validate either and record which transport worked, so the
// proxy dials it the same way (issue #31).
func TestValidateEndpoint_GRPCRecordsTransport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		serverTLS    *tls.Config
		wantInsecure bool
	}{
		{name: "plaintext endpoint", serverTLS: nil, wantInsecure: true},
		{name: "TLS endpoint", serverTLS: testServerTLS, wantInsecure: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			addr, stop, err := testtls.StartBlockServer(tt.serverTLS, 976400)
			if err != nil {
				t.Fatalf("start server: %v", err)
			}
			t.Cleanup(stop)

			c, _, store := newTestExternalChecker(t, nil)
			t.Cleanup(c.Close)
			store.StoreAdvertised("ring-a", "https://ring-a:3000", "pocket", "grpc", addr)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c.validateEndpoint(ctx, "ring-a", "https://ring-a:3000", "pocket", "grpc", addr, 976400)

			eps := store.GetValidatedEndpoints("pocket", "grpc")
			if len(eps) != 1 || eps[0].URL != addr {
				t.Fatalf("validated endpoints: got %+v, want %s", eps, addr)
			}
			insecure, known := store.GRPCTransport(addr)
			if !known || insecure != tt.wantInsecure {
				t.Fatalf("GRPCTransport(%s): got insecure=%v known=%v, want insecure=%v known=true", addr, insecure, known, tt.wantInsecure)
			}
		})
	}
}

// A FAILED TLS endpoint must recover through RecoverFailedEndpoints, which
// validated in plaintext only.
func TestRecoverFailedEndpoints_TLSEndpointRecovers(t *testing.T) {
	t.Parallel()

	addr, stop, err := testtls.StartBlockServer(testServerTLS, 976400)
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(stop)

	c, _, store := newTestExternalChecker(t, nil)
	t.Cleanup(c.Close)
	store.StoreAdvertised("ring-a", "https://ring-a:3000", "pocket", "grpc", addr)
	store.MarkValidated("ring-a", "https://ring-a:3000", "pocket", "grpc", addr, 976400, time.Millisecond)
	store.MarkValidationFailed("ring-a", "https://ring-a:3000", "pocket", "grpc", addr)
	if n := len(store.GetFailedEndpoints()); n != 1 {
		t.Fatalf("setup: %d failed endpoints, want 1", n)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.RecoverFailedEndpoints(ctx)

	if n := len(store.GetFailedEndpoints()); n != 0 {
		t.Fatalf("after recovery: %d failed endpoints, want 0", n)
	}
	if insecure, known := store.GRPCTransport(addr); !known || insecure {
		t.Fatalf("GRPCTransport: got insecure=%v known=%v, want TLS", insecure, known)
	}
}
