package grpcutil_test

import (
	"strings"
	"testing"

	"sauron/internal/grpcutil"
)

// TestNewConnection_PassthroughResolver verifies that NewConnection correctly prepends
// "passthrough:///" when the target has no recognized resolver prefix.
func TestNewConnection_PassthroughResolver(t *testing.T) {
	tests := []struct {
		name            string
		target          string
		wantPassthrough bool
	}{
		{
			name:            "bare host:port gets passthrough prefix",
			target:          "example.com:9090",
			wantPassthrough: true,
		},
		{
			name:            "already has passthrough prefix — not double-wrapped",
			target:          "passthrough:///example.com:9090",
			wantPassthrough: false, // no additional wrapping needed
		},
		{
			name:            "dns:// prefix — not wrapped",
			target:          "dns://authority/example.com:9090",
			wantPassthrough: false,
		},
		{
			name:            "bare IP address gets passthrough prefix",
			target:          "1.2.3.4:9090",
			wantPassthrough: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// We can't easily inspect the resolved target after NewConnection returns
			// a *grpc.ClientConn, but we can verify the logic by calling NewConnection
			// and ensuring it does not error (connection is lazy, so no network call occurs).
			conn, err := grpcutil.NewConnection(tc.target, true)
			if err != nil {
				t.Fatalf("NewConnection(%q, true) returned unexpected error: %v", tc.target, err)
			}
			defer func() { _ = conn.Close() }()

			// Use the target field indirectly: the conn target should reflect resolved target.
			connTarget := conn.Target()
			if tc.wantPassthrough {
				if !strings.HasPrefix(connTarget, "passthrough:///") {
					t.Errorf("expected conn.Target() to start with passthrough:///, got %q", connTarget)
				}
			} else {
				// Already had a resolver prefix; should not be double-wrapped
				pfx := strings.Count(connTarget, "passthrough:///")
				if pfx > 1 {
					t.Errorf("target was double-wrapped with passthrough: %q", connTarget)
				}
			}
		})
	}
}

// TestNewConnection_InsecureVsTLS verifies both credential paths don't error at dial time.
func TestNewConnection_InsecureVsTLS(t *testing.T) {
	t.Run("insecure", func(t *testing.T) {
		conn, err := grpcutil.NewConnection("localhost:50051", true)
		if err != nil {
			t.Fatalf("NewConnection insecure returned error: %v", err)
		}
		_ = conn.Close()
	})

	t.Run("TLS", func(t *testing.T) {
		conn, err := grpcutil.NewConnection("localhost:50051", false)
		if err != nil {
			t.Fatalf("NewConnection TLS returned error: %v", err)
		}
		_ = conn.Close()
	})
}
