package proxy

import (
	"sync"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

// ---------------------------------------------------------------------------
// TestNormalizeGRPCMethod
// ---------------------------------------------------------------------------

func TestNormalizeGRPCMethod(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "standard full path",
			input: "/pocket.session.Query/GetSession",
			want:  "pocket.session.Query",
		},
		{
			name:  "no leading slash",
			input: "pocket.session.Query/GetSession",
			want:  "pocket.session.Query",
		},
		{
			name:  "deeply nested package",
			input: "/a.b.c.Service/Method",
			want:  "a.b.c.Service",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "only slash",
			input: "/",
			want:  "",
		},
		{
			name:  "no slash at all",
			input: "NoService",
			want:  "NoService",
		},
		{
			name:  "only method part after leading slash",
			input: "/Method",
			want:  "Method",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeGRPCMethod(tc.input)
			if got != tc.want {
				t.Errorf("normalizeGRPCMethod(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers for connection pool tests
// ---------------------------------------------------------------------------

// newTestGRPCProxy creates a minimal *GRPCProxy suitable for unit tests that
// only exercise the connection pool; it does NOT start a gRPC server.
func newTestGRPCProxy(t *testing.T) *GRPCProxy {
	t.Helper()
	cfgLoader := createTestConfigLoader(t)
	return &GRPCProxy{
		configLoader: cfgLoader,
		logger:       zap.NewNop(),
		network:      "testnet",
		connPool:     make(map[string]*grpc.ClientConn),
	}
}

// dialInsecure is a small helper that creates a grpc.ClientConn to a
// passthrough address using insecure credentials. Nothing is listening at the
// address; grpc.NewClient does not actually dial until a call is made, so this
// succeeds immediately.
func dialInsecure(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	target := "passthrough:///" + addr
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialInsecure(%s): %v", addr, err)
	}
	return conn
}

// ---------------------------------------------------------------------------
// TestGetOrCreateConnection_ClosesShutdownConnection (H4)
// ---------------------------------------------------------------------------

// TestGetOrCreateConnection_ClosesShutdownConnection verifies that when the
// cached connection for an address is in SHUTDOWN state the next call to
// getOrCreateConnection:
//  1. removes the dead connection from the pool
//  2. returns a newly created connection that is NOT the old one
func TestGetOrCreateConnection_ClosesShutdownConnection(t *testing.T) {
	p := newTestGRPCProxy(t)

	addr := "127.0.0.1:19990"

	// Create a real connection and immediately close it so it enters SHUTDOWN.
	deadConn := dialInsecure(t, addr)
	_ = deadConn.Close() // drives state → SHUTDOWN

	// Verify it really is SHUTDOWN before seeding.
	if deadConn.GetState() != connectivity.Shutdown {
		t.Skipf("connection did not reach SHUTDOWN (got %s); skipping", deadConn.GetState())
	}

	// Seed the pool with the dead connection.
	p.connMu.Lock()
	p.connPool[addr] = deadConn
	p.connMu.Unlock()

	// Ask the proxy for a connection — it must detect SHUTDOWN and return a fresh one.
	newConn, err := p.getOrCreateConnection(addr, true /*insecure*/)
	if err != nil {
		t.Fatalf("getOrCreateConnection: %v", err)
	}
	defer func() { _ = newConn.Close() }()

	// The returned connection must not be the dead one.
	if newConn == deadConn {
		t.Error("getOrCreateConnection returned the same SHUTDOWN connection; expected a new one")
	}

	// The pool must now hold the new connection, not the dead one.
	p.connMu.RLock()
	pooled := p.connPool[addr]
	p.connMu.RUnlock()

	if pooled == deadConn {
		t.Error("connPool still holds the SHUTDOWN connection after replacement")
	}
	if pooled != newConn {
		t.Error("connPool does not hold the newly created connection")
	}

	// The new connection must not be in SHUTDOWN state.
	if pooled.GetState() == connectivity.Shutdown {
		t.Errorf("new pooled connection is already in SHUTDOWN: %s", pooled.GetState())
	}
}

// ---------------------------------------------------------------------------
// TestGetOrCreateConnection_ConcurrentAccess (race detector)
// ---------------------------------------------------------------------------

// TestGetOrCreateConnection_ConcurrentAccess runs 50 goroutines all requesting
// the same target address concurrently. The race detector flags any locking
// defects in getOrCreateConnection.
func TestGetOrCreateConnection_ConcurrentAccess(t *testing.T) {
	p := newTestGRPCProxy(t)
	addr := "127.0.0.1:19991"

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			conn, err := p.getOrCreateConnection(addr, true /*insecure*/)
			if err != nil {
				errs <- err
				return
			}
			_ = conn
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("getOrCreateConnection error: %v", err)
	}

	// Clean up via the proxy's Close — must not panic or race.
	if err := p.Close(); err != nil {
		t.Logf("p.Close() returned error (benign on non-listening addr): %v", err)
	}

	// Second Close must be a no-op (idempotent — H8 requirement).
	if err := p.Close(); err != nil {
		t.Logf("p.Close() second call returned error: %v", err)
	}
}
