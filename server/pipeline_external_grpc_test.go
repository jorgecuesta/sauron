package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	tmservice "cosmossdk.io/api/cosmos/base/tendermint/v1beta1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"sauron/internal/testtls"
)

// testServerTLS presents a certificate the process trusts (SSL_CERT_FILE is
// set before any test runs).
var testServerTLS *tls.Config

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sauron-server-tls-")
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

const externalOnlyYAML = `
listen: "%s"
timeouts:
  health_check: 2s
  proxy: 10s
networks:
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: grpc
        listen: "%s"
externals:
  - name: %s
    rings: ["%s"]
`

// TestPipeline_ExternalGRPCEndpoint: a Sauron with no internal nodes routes a
// gRPC request to the endpoint an external ring advertises, whether that
// endpoint speaks plaintext or TLS (issue #31: validation used plaintext and
// the proxy TLS, so neither worked).
func TestPipeline_ExternalGRPCEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		serverTLS *tls.Config
	}{
		{name: "plaintext", serverTLS: nil},
		{name: "tls", serverTLS: testServerTLS},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backend, stop, err := testtls.StartBlockServer(tt.serverTLS, 976400)
			if err != nil {
				t.Fatalf("start backend: %v", err)
			}
			t.Cleanup(stop)

			ring := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/pocket/status" {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"height":    976400,
					"endpoints": map[string]string{"grpc": backend},
				})
			}))
			t.Cleanup(ring.Close)

			status, grpcAddr := freeAddr(t), freeAddr(t)
			ringName := fmt.Sprintf("ext-grpc-%d-%d", pipelineSeq.Add(1), i)
			path := t.TempDir() + "/config.yaml"
			if err := os.WriteFile(path, []byte(fmt.Sprintf(externalOnlyYAML, status, grpcAddr, ringName, ring.URL)), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			s, err := New(path)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := s.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(s.Shutdown)

			conn, err := grpc.NewClient("passthrough:///"+grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatalf("dial sauron: %v", err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			client := tmservice.NewServiceClient(conn)

			// External rings are read every 10s; allow two rounds.
			deadline := time.Now().Add(25 * time.Second)
			var lastErr error
			for time.Now().Before(deadline) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				resp, err := client.GetLatestBlock(ctx, &tmservice.GetLatestBlockRequest{})
				cancel()
				if err == nil {
					if h := resp.GetSdkBlock().GetHeader().GetHeight(); h != 976400 {
						t.Fatalf("height through sauron: got %d, want 976400", h)
					}
					return
				}
				lastErr = err
				time.Sleep(250 * time.Millisecond)
			}
			t.Fatalf("no gRPC answer through the external endpoint within 25s; last error: %v", lastErr)
		})
	}
}
