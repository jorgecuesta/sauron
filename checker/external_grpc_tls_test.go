package checker

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	tmservice "cosmossdk.io/api/cosmos/base/tendermint/v1beta1"
	"google.golang.org/grpc"

	"sauron/internal/grpcutil"
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

// After a plaintext endpoint validates, only its plaintext connection stays
// cached: the failed TLS attempt's connection is closed, not left reconnecting.
func TestValidateGRPCAnyTransport_DropsLosingConnection(t *testing.T) {
	t.Parallel()

	addr, stop, err := testtls.StartBlockServer(nil, 976400)
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(stop)
	c, _, _ := newTestExternalChecker(t, nil)
	t.Cleanup(c.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, insecure, err := c.validateGRPCAnyTransport(ctx, addr); err != nil || !insecure {
		t.Fatalf("validate: insecure=%v err=%v, want plaintext", insecure, err)
	}
	if _, ok := c.grpcConnections.Load(grpcutil.ConnKey(addr, false)); ok {
		t.Error("the failed TLS connection is still cached")
	}
	if _, ok := c.grpcConnections.Load(grpcutil.ConnKey(addr, true)); !ok {
		t.Error("the working plaintext connection is not cached")
	}
}

// stallTLSListener hands plaintext connections to the server and holds TLS
// ones (first byte 0x16, a handshake record) open without answering.
type stallTLSListener struct {
	net.Listener
	mu   sync.Mutex
	held []net.Conn
}

func (l *stallTLSListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		br := bufio.NewReader(conn)
		b, err := br.Peek(1)
		if err != nil {
			_ = conn.Close()
			continue
		}
		if b[0] == 0x16 {
			l.mu.Lock()
			l.held = append(l.held, conn) // never answered
			l.mu.Unlock()
			continue
		}
		return &peekedConn{Conn: conn, r: br}, nil
	}
}

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// A TLS attempt that stalls must not use the whole check deadline: the
// plaintext attempt still gets time and the endpoint validates.
func TestValidateGRPCAnyTransport_StalledFirstAttemptLeavesTime(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	sl := &stallTLSListener{Listener: ln}
	t.Cleanup(func() {
		sl.mu.Lock()
		defer sl.mu.Unlock()
		for _, c := range sl.held {
			_ = c.Close()
		}
	})
	srv := grpc.NewServer()
	tmservice.RegisterServiceServer(srv, &fakeTMService{})
	go func() { _ = srv.Serve(sl) }()
	t.Cleanup(srv.Stop)

	c, _, _ := newTestExternalChecker(t, nil)
	t.Cleanup(c.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if _, insecure, err := c.validateGRPCAnyTransport(ctx, ln.Addr().String()); err != nil || !insecure {
		t.Fatalf("validate: insecure=%v err=%v, want plaintext within the deadline", insecure, err)
	}
}
