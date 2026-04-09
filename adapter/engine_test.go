package adapter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"sauron/internal/jsonpath"
)

func TestEngine_CheckHeight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		response   string
		status     int
		cfg        CheckConfig
		wantHeight int64
		wantErr    bool
	}{
		{
			name:     "cosmos rpc height",
			response: `{"result":{"sync_info":{"latest_block_height":"58272941"}}}`,
			status:   200,
			cfg: CheckConfig{
				Method:         "GET",
				URLPath:        "/status",
				ResponsePath:   ".result.sync_info.latest_block_height",
				ResponseFormat: "string",
				Protocol:       "rpc",
			},
			wantHeight: 58272941,
		},
		{
			name:     "evm hex height",
			response: `{"jsonrpc":"2.0","id":1,"result":"0x1312d00"}`,
			status:   200,
			cfg: CheckConfig{
				Method:         "POST",
				URLPath:        "/",
				Headers:        http.Header{"Content-Type": {"application/json"}},
				Body:           []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`),
				ResponsePath:   ".result",
				ResponseFormat: "hex",
				Protocol:       "jsonrpc",
			},
			wantHeight: 20000000,
		},
		{
			name:     "solana integer height",
			response: `{"jsonrpc":"2.0","id":1,"result":300000000}`,
			status:   200,
			cfg: CheckConfig{
				Method:         "POST",
				URLPath:        "/",
				ResponsePath:   ".result",
				ResponseFormat: "integer",
				Protocol:       "jsonrpc",
			},
			wantHeight: 300000000,
		},
		{
			name:     "graphql nested height",
			response: `{"data":{"bestChain":[{"protocolState":{"consensusState":{"blockHeight":"395000"}}}]}}`,
			status:   200,
			cfg: CheckConfig{
				Method:         "POST",
				URLPath:        "/graphql",
				Headers:        http.Header{"Content-Type": {"application/json"}},
				Body:           []byte(`{"query":"{ bestChain(maxLength:1) { protocolState { consensusState { blockHeight } } } }"}`),
				ResponsePath:   ".data.bestChain[0].protocolState.consensusState.blockHeight",
				ResponseFormat: "integer",
				Protocol:       "http",
			},
			wantHeight: 395000,
		},
		{
			name:     "server error",
			response: "internal server error",
			status:   500,
			cfg: CheckConfig{
				Method:       "GET",
				URLPath:      "/status",
				ResponsePath: ".result",
				Protocol:     "rpc",
			},
			wantErr: true,
		},
		{
			name:     "invalid json response",
			response: "not json at all",
			status:   200,
			cfg: CheckConfig{
				Method:       "GET",
				URLPath:      "/status",
				ResponsePath: ".result",
				Protocol:     "rpc",
			},
			wantErr: true,
		},
		{
			name:     "path not found in response",
			response: `{"other": "data"}`,
			status:   200,
			cfg: CheckConfig{
				Method:       "GET",
				URLPath:      "/status",
				ResponsePath: ".result.sync_info.height",
				Protocol:     "rpc",
			},
			wantErr: true,
		},
		{
			name:     "null height value",
			response: `{"result": null}`,
			status:   200,
			cfg: CheckConfig{
				Method:       "GET",
				URLPath:      "/check",
				ResponsePath: ".result",
				Protocol:     "http",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tt.cfg.URLPath {
					t.Errorf("unexpected path: got %q, want %q", r.URL.Path, tt.cfg.URLPath)
				}
				if r.Method != tt.cfg.Method {
					t.Errorf("unexpected method: got %q, want %q", r.Method, tt.cfg.Method)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.response))
			}))
			defer srv.Close()

			engine := NewEngine(srv.Client())
			height, err := engine.CheckHeight(context.Background(), tt.cfg, srv.URL)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if height != tt.wantHeight {
				t.Fatalf("got height %d, want %d", height, tt.wantHeight)
			}
		})
	}
}

func TestEngine_CheckHeight_GRPC(t *testing.T) {
	t.Parallel()
	engine := NewEngine(http.DefaultClient)
	_, err := engine.CheckHeight(context.Background(), CheckConfig{IsGRPC: true}, "localhost:9090")
	if err == nil {
		t.Fatal("expected error for gRPC height check")
	}
}

func TestEngine_CheckHeight_ContextCanceled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"height": 1}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	engine := NewEngine(srv.Client())
	_, err := engine.CheckHeight(ctx, CheckConfig{
		Method:       "GET",
		URLPath:      "/status",
		ResponsePath: ".height",
		Protocol:     "rpc",
	}, srv.URL)
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}

func TestEngine_CheckHealth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		cfg     HealthCheckConfig
		wantErr bool
	}{
		{
			name:   "healthy 200",
			status: 200,
			cfg: HealthCheckConfig{
				Protocol: "rest",
				Method:   "GET",
				URLPath:  "/health",
			},
		},
		{
			name:   "healthy custom status",
			status: 204,
			cfg: HealthCheckConfig{
				Protocol:     "rest",
				Method:       "GET",
				URLPath:      "/health",
				ExpectStatus: 204,
			},
		},
		{
			name:   "unhealthy 500",
			status: 500,
			cfg: HealthCheckConfig{
				Protocol: "rest",
				Method:   "GET",
				URLPath:  "/health",
			},
			wantErr: true,
		},
		{
			name:   "unhealthy wrong status",
			status: 503,
			cfg: HealthCheckConfig{
				Protocol:     "rest",
				Method:       "GET",
				URLPath:      "/health",
				ExpectStatus: 200,
			},
			wantErr: true,
		},
		{
			name: "grpc not implemented",
			cfg: HealthCheckConfig{
				Protocol: "grpc",
				IsGRPC:   true,
			},
			wantErr: true,
		},
		{
			name: "websocket not implemented",
			cfg: HealthCheckConfig{
				Protocol:    "websocket",
				IsWebSocket: true,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			engine := NewEngine(srv.Client())
			err := engine.CheckHealth(context.Background(), tt.cfg, srv.URL)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestEngine_CheckHealth_WithBody(t *testing.T) {
	t.Parallel()

	var gotBody bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > 0 {
			gotBody = true
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	engine := NewEngine(srv.Client())
	err := engine.CheckHealth(context.Background(), HealthCheckConfig{
		Protocol: "jsonrpc",
		Method:   "POST",
		URLPath:  "/",
		Body:     []byte(`{"jsonrpc":"2.0","method":"net_version","params":[],"id":1}`),
	}, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gotBody {
		t.Fatal("expected request body to be sent")
	}
}

func TestEngine_CheckHeight_HeadersSent(t *testing.T) {
	t.Parallel()

	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"result": 42}`))
	}))
	defer srv.Close()

	engine := NewEngine(srv.Client())
	_, err := engine.CheckHeight(context.Background(), CheckConfig{
		Method:       "POST",
		URLPath:      "/",
		Headers:      http.Header{"Content-Type": {"application/json"}},
		Body:         []byte(`{}`),
		ResponsePath: ".result",
		Protocol:     "jsonrpc",
	}, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type not sent: got %q", gotContentType)
	}
}

func TestEngine_CheckHeight_EmptyResponseBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		// Empty body — valid HTTP but invalid JSON.
	}))
	defer srv.Close()

	engine := NewEngine(srv.Client())
	_, err := engine.CheckHeight(context.Background(), CheckConfig{
		Method:       "GET",
		URLPath:      "/",
		ResponsePath: ".height",
		Protocol:     "rpc",
	}, srv.URL)
	if err == nil {
		t.Fatal("expected error for empty response body")
	}
}

func TestEngine_CheckHeight_InvalidURL(t *testing.T) {
	t.Parallel()

	engine := NewEngine(&http.Client{})
	_, err := engine.CheckHeight(context.Background(), CheckConfig{
		Method:       "GET",
		URLPath:      "/status",
		ResponsePath: ".height",
		Protocol:     "rpc",
	}, "http://127.0.0.1:0") // Port 0 — connection refused.
	if err == nil {
		t.Fatal("expected error for unreachable host")
	}
}

func TestEngine_CheckHealth_ContextCanceled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	engine := NewEngine(srv.Client())
	err := engine.CheckHealth(ctx, HealthCheckConfig{
		Protocol: "rest",
		Method:   "GET",
		URLPath:  "/health",
	}, srv.URL)
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}

func TestEngine_CheckHealth_HeadersSent(t *testing.T) {
	t.Parallel()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	engine := NewEngine(srv.Client())
	err := engine.CheckHealth(context.Background(), HealthCheckConfig{
		Protocol: "rest",
		Method:   "GET",
		URLPath:  "/health",
		Headers:  http.Header{"Authorization": {"Bearer token123"}},
	}, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer token123" {
		t.Fatalf("Authorization header not sent: got %q", gotAuth)
	}
}

func TestEngine_CheckHealth_InvalidURL(t *testing.T) {
	t.Parallel()

	engine := NewEngine(&http.Client{})
	err := engine.CheckHealth(context.Background(), HealthCheckConfig{
		Protocol: "rest",
		Method:   "GET",
		URLPath:  "/health",
	}, "http://127.0.0.1:0")
	if err == nil {
		t.Fatal("expected error for unreachable host")
	}
}

// Verify jsonpath errors propagate correctly.
func TestEngine_CheckHeight_JSONPathError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"height": true}`))
	}))
	defer srv.Close()

	engine := NewEngine(srv.Client())
	_, err := engine.CheckHeight(context.Background(), CheckConfig{
		Method:       "GET",
		URLPath:      "/",
		ResponsePath: ".height",
		Protocol:     "rpc",
	}, srv.URL)
	if err == nil {
		t.Fatal("expected error for boolean height")
	}
	if !errors.Is(err, jsonpath.ErrType) {
		t.Fatalf("expected jsonpath.ErrType, got: %v", err)
	}
}
