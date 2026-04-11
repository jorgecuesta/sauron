package custom

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"sauron/adapter"
)

func TestFactory_Type(t *testing.T) {
	t.Parallel()
	if got := New().Type(); got != "custom" {
		t.Fatalf("got %q, want custom", got)
	}
}

// --- HeightCheck tests ---

func TestFactory_HeightCheck_GraphQL(t *testing.T) {
	t.Parallel()
	f := New()

	// Mina-like GraphQL config from the plan.
	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "mina",
		Type: "custom",
		HeightCheck: &adapter.HeightCheckOverride{
			Method:         "POST",
			URLPath:        "/graphql",
			Headers:        map[string]string{"Content-Type": "application/json"},
			Body:           `{"query":"{ bestChain(maxLength:1) { protocolState { consensusState { blockHeight } } } }"}`,
			ResponsePath:   ".data.bestChain[0].protocolState.consensusState.blockHeight",
			ResponseFormat: "integer",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Method != "POST" {
		t.Fatalf("Method: got %q", cfg.Method)
	}
	if cfg.URLPath != "/graphql" {
		t.Fatalf("URLPath: got %q", cfg.URLPath)
	}
	if cfg.Headers.Get("Content-Type") != "application/json" {
		t.Fatal("missing Content-Type header")
	}
	if cfg.ResponsePath != ".data.bestChain[0].protocolState.consensusState.blockHeight" {
		t.Fatalf("ResponsePath: got %q", cfg.ResponsePath)
	}
	if cfg.ResponseFormat != "integer" {
		t.Fatalf("ResponseFormat: got %q", cfg.ResponseFormat)
	}
	if string(cfg.Body) == "" {
		t.Fatal("expected non-empty body")
	}
}

func TestFactory_HeightCheck_SimpleGET(t *testing.T) {
	t.Parallel()
	f := New()

	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "mychain",
		Type: "custom",
		HeightCheck: &adapter.HeightCheckOverride{
			Method:       "GET",
			URLPath:      "/api/v1/height",
			ResponsePath: ".height",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Method != "GET" {
		t.Fatalf("Method: got %q", cfg.Method)
	}
	if cfg.URLPath != "/api/v1/height" {
		t.Fatalf("URLPath: got %q", cfg.URLPath)
	}
	// Default format should be "integer".
	if cfg.ResponseFormat != "integer" {
		t.Fatalf("ResponseFormat: got %q, want integer", cfg.ResponseFormat)
	}
	// Default protocol should be "http".
	if cfg.Protocol != "http" {
		t.Fatalf("Protocol: got %q, want http", cfg.Protocol)
	}
}

func TestFactory_HeightCheck_HexFormat(t *testing.T) {
	t.Parallel()
	f := New()

	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "evmfork",
		Type: "custom",
		HeightCheck: &adapter.HeightCheckOverride{
			Method:         "POST",
			URLPath:        "/",
			Body:           `{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`,
			ResponsePath:   ".result",
			ResponseFormat: "hex",
			Protocol:       "jsonrpc",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.ResponseFormat != "hex" {
		t.Fatalf("ResponseFormat: got %q", cfg.ResponseFormat)
	}
	if cfg.Protocol != "jsonrpc" {
		t.Fatalf("Protocol: got %q", cfg.Protocol)
	}
}

func TestFactory_HeightCheck_CustomHeaders(t *testing.T) {
	t.Parallel()
	f := New()

	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "authchain",
		Type: "custom",
		HeightCheck: &adapter.HeightCheckOverride{
			Method:       "GET",
			URLPath:      "/height",
			ResponsePath: ".h",
			Headers: map[string]string{
				"Authorization": "Bearer secret",
				"X-Custom":      "value",
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Headers.Get("Authorization") != "Bearer secret" {
		t.Fatal("missing Authorization header")
	}
	if cfg.Headers.Get("X-Custom") != "value" {
		t.Fatal("missing X-Custom header")
	}
}

func TestFactory_HeightCheck_NilOverride(t *testing.T) {
	t.Parallel()
	f := New()

	_, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "bad", Type: "custom",
	})
	if err == nil {
		t.Fatal("expected error for nil HeightCheck")
	}
}

func TestFactory_HeightCheck_MissingMethod(t *testing.T) {
	t.Parallel()
	f := New()

	_, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "bad", Type: "custom",
		HeightCheck: &adapter.HeightCheckOverride{
			ResponsePath: ".height",
		},
	})
	if err == nil {
		t.Fatal("expected error for missing method")
	}
}

func TestFactory_HeightCheck_MissingResponsePath(t *testing.T) {
	t.Parallel()
	f := New()

	_, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "bad", Type: "custom",
		HeightCheck: &adapter.HeightCheckOverride{
			Method: "GET",
		},
	})
	if err == nil {
		t.Fatal("expected error for missing response_path")
	}
}

// --- HealthChecks tests ---

func TestFactory_HealthChecks_HTTP(t *testing.T) {
	t.Parallel()
	f := New()
	checks, err := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Endpoint: map[string]string{"http": "http://node:3085"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("expected 1, got %d", len(checks))
	}
	if checks[0].Protocol != "http" {
		t.Fatalf("Protocol: got %q", checks[0].Protocol)
	}
	if checks[0].Method != "GET" {
		t.Fatalf("Method: got %q", checks[0].Method)
	}
}

func TestFactory_HealthChecks_WebSocket(t *testing.T) {
	t.Parallel()
	f := New()
	checks, err := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Endpoint: map[string]string{"websocket": "ws://node:3086"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(checks) != 1 || !checks[0].IsWebSocket {
		t.Fatal("expected websocket health check")
	}
}

func TestFactory_HealthChecks_UnknownIgnored(t *testing.T) {
	t.Parallel()
	f := New()
	checks, err := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Endpoint: map[string]string{"grpc": "node:9090"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("expected 0 (grpc ignored), got %d", len(checks))
	}
}

func TestFactory_HealthChecks_Empty(t *testing.T) {
	t.Parallel()
	f := New()
	checks, err := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Endpoint: map[string]string{},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("expected 0, got %d", len(checks))
	}
}

func TestFactory_HealthChecks_EmptyURLSkipped(t *testing.T) {
	t.Parallel()
	f := New()
	checks, err := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Endpoint: map[string]string{"http": "", "websocket": ""},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("expected 0 checks for empty URLs, got %d", len(checks))
	}
}

// --- Integration: Factory → Engine with Mina-like GraphQL ---

func TestIntegration_FactoryToEngine_GraphQL(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":{"bestChain":[{"protocolState":{"consensusState":{"blockHeight":"395000"}}}]}}`))
	}))
	defer srv.Close()

	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "mina", Type: "custom",
		HeightCheck: &adapter.HeightCheckOverride{
			Method:         "POST",
			URLPath:        "/graphql",
			Headers:        map[string]string{"Content-Type": "application/json"},
			Body:           `{"query":"{ bestChain(maxLength:1) { protocolState { consensusState { blockHeight } } } }"}`,
			ResponsePath:   ".data.bestChain[0].protocolState.consensusState.blockHeight",
			ResponseFormat: "integer",
		},
	})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}

	engine := adapter.NewEngine(srv.Client())
	height, err := engine.CheckHeight(context.Background(), cfg, srv.URL)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if height != 395000 {
		t.Fatalf("got %d, want 395000", height)
	}
}

func TestIntegration_FactoryToEngine_SimpleGET(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"height": 12345}`))
	}))
	defer srv.Close()

	f := New()
	cfg, _ := f.HeightCheck(adapter.NetworkConfig{
		Name: "mychain", Type: "custom",
		HeightCheck: &adapter.HeightCheckOverride{
			Method:       "GET",
			URLPath:      "/height",
			ResponsePath: ".height",
		},
	})

	engine := adapter.NewEngine(srv.Client())
	height, err := engine.CheckHeight(context.Background(), cfg, srv.URL)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if height != 12345 {
		t.Fatalf("got %d, want 12345", height)
	}
}

func TestIntegration_FactoryToEngine_HexCustom(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"result":"0xff"}`))
	}))
	defer srv.Close()

	f := New()
	cfg, _ := f.HeightCheck(adapter.NetworkConfig{
		Name: "evmfork", Type: "custom",
		HeightCheck: &adapter.HeightCheckOverride{
			Method:         "POST",
			URLPath:        "/",
			ResponsePath:   ".result",
			ResponseFormat: "hex",
		},
	})

	engine := adapter.NewEngine(srv.Client())
	height, err := engine.CheckHeight(context.Background(), cfg, srv.URL)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if height != 255 { // 0xff
		t.Fatalf("got %d, want 255", height)
	}
}

// --- ArchivalCheck tests ---

// TestFactory_ArchivalCheck_Passthrough verifies that ArchivalCheck delegates to HeightCheck,
// returning the same config regardless of minHeight.
func TestFactory_ArchivalCheck_Passthrough(t *testing.T) {
	t.Parallel()

	net := adapter.NetworkConfig{
		Name: "mina",
		Type: "custom",
		HeightCheck: &adapter.HeightCheckOverride{
			Method:         "POST",
			URLPath:        "/graphql",
			Headers:        map[string]string{"Content-Type": "application/json"},
			Body:           `{"query":"{ bestChain(maxLength:1) { protocolState { consensusState { blockHeight } } } }"}`,
			ResponsePath:   ".data.bestChain[0].protocolState.consensusState.blockHeight",
			ResponseFormat: "integer",
		},
	}

	f := New()
	heightCfg, err := f.HeightCheck(net)
	if err != nil {
		t.Fatalf("HeightCheck: unexpected error: %v", err)
	}

	tests := []struct {
		name      string
		minHeight int64
	}{
		{name: "height 1", minHeight: 1},
		{name: "height 1000000", minHeight: 1000000},
		{name: "height 0", minHeight: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			archivalCfg, err := f.ArchivalCheck(net, tt.minHeight)
			if err != nil {
				t.Fatalf("ArchivalCheck: unexpected error: %v", err)
			}

			if archivalCfg.Method != heightCfg.Method {
				t.Fatalf("Method: archival=%q, height=%q", archivalCfg.Method, heightCfg.Method)
			}
			if archivalCfg.URLPath != heightCfg.URLPath {
				t.Fatalf("URLPath: archival=%q, height=%q", archivalCfg.URLPath, heightCfg.URLPath)
			}
			if archivalCfg.ResponsePath != heightCfg.ResponsePath {
				t.Fatalf("ResponsePath: archival=%q, height=%q", archivalCfg.ResponsePath, heightCfg.ResponsePath)
			}
			if archivalCfg.ResponseFormat != heightCfg.ResponseFormat {
				t.Fatalf("ResponseFormat: archival=%q, height=%q", archivalCfg.ResponseFormat, heightCfg.ResponseFormat)
			}
			if archivalCfg.Protocol != heightCfg.Protocol {
				t.Fatalf("Protocol: archival=%q, height=%q", archivalCfg.Protocol, heightCfg.Protocol)
			}
			if string(archivalCfg.Body) != string(heightCfg.Body) {
				t.Fatalf("Body: archival=%q, height=%q", archivalCfg.Body, heightCfg.Body)
			}
		})
	}
}

// TestFactory_ArchivalCheck_NilOverride verifies that ArchivalCheck propagates HeightCheck errors.
func TestFactory_ArchivalCheck_NilOverride(t *testing.T) {
	t.Parallel()
	f := New()
	_, err := f.ArchivalCheck(adapter.NetworkConfig{Name: "bad", Type: "custom"}, 1000)
	if err == nil {
		t.Fatal("expected error for nil HeightCheck override")
	}
}

// TestFactory_ArchivalCheck_MinHeightIgnored verifies that different minHeight values all
// return the same config (minHeight is irrelevant for the custom passthrough).
func TestFactory_ArchivalCheck_MinHeightIgnored(t *testing.T) {
	t.Parallel()
	f := New()
	net := adapter.NetworkConfig{
		Name: "mychain",
		Type: "custom",
		HeightCheck: &adapter.HeightCheckOverride{
			Method:       "GET",
			URLPath:      "/api/v1/height",
			ResponsePath: ".height",
		},
	}

	cfg1, err := f.ArchivalCheck(net, 1)
	if err != nil {
		t.Fatalf("ArchivalCheck(1): %v", err)
	}
	cfg2, err := f.ArchivalCheck(net, 999999)
	if err != nil {
		t.Fatalf("ArchivalCheck(999999): %v", err)
	}

	// Both calls must produce identical configs.
	if cfg1.URLPath != cfg2.URLPath {
		t.Fatalf("URLPath differs: %q vs %q", cfg1.URLPath, cfg2.URLPath)
	}
	if cfg1.ResponsePath != cfg2.ResponsePath {
		t.Fatalf("ResponsePath differs: %q vs %q", cfg1.ResponsePath, cfg2.ResponsePath)
	}
}

// Verify Factory satisfies ChainAdapter interface.
var _ adapter.ChainAdapter = (*Factory)(nil)
