package solana

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"sauron/adapter"
)

func TestFactory_Type(t *testing.T) {
	t.Parallel()
	if got := New().Type(); got != "solana" {
		t.Fatalf("got %q, want solana", got)
	}
}

// --- HeightCheck tests ---

func TestFactory_HeightCheck_DefaultGetSlotFinalized(t *testing.T) {
	t.Parallel()
	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{Name: "solana", Type: "solana"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Method != "POST" {
		t.Fatalf("Method: got %q", cfg.Method)
	}
	if cfg.ResponsePath != ".result" {
		t.Fatalf("ResponsePath: got %q", cfg.ResponsePath)
	}
	if cfg.ResponseFormat != "integer" {
		t.Fatalf("ResponseFormat: got %q, want integer", cfg.ResponseFormat)
	}
	if cfg.Protocol != "jsonrpc" {
		t.Fatalf("Protocol: got %q", cfg.Protocol)
	}

	var rpc jsonRPCRequest
	if err := json.Unmarshal(cfg.Body, &rpc); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if rpc.Method != "getSlot" {
		t.Fatalf("RPC method: got %q, want getSlot", rpc.Method)
	}
	// Verify commitment param.
	assertCommitment(t, rpc.Params, "finalized")
}

func TestFactory_HeightCheck_GetSlotConfirmed(t *testing.T) {
	t.Parallel()
	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "solana", Type: "solana", Mode: "confirmed",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var rpc jsonRPCRequest
	if err := json.Unmarshal(cfg.Body, &rpc); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if rpc.Method != "getSlot" {
		t.Fatalf("RPC method: got %q, want getSlot", rpc.Method)
	}
	assertCommitment(t, rpc.Params, "confirmed")
}

func TestFactory_HeightCheck_GetSlotProcessed(t *testing.T) {
	t.Parallel()
	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "solana", Type: "solana", Mode: "processed",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var rpc jsonRPCRequest
	if err := json.Unmarshal(cfg.Body, &rpc); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	assertCommitment(t, rpc.Params, "processed")
}

func TestFactory_HeightCheck_GetBlockHeight(t *testing.T) {
	t.Parallel()
	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "solana", Type: "solana",
		HeightCheck: &adapter.HeightCheckOverride{Protocol: "getBlockHeight"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var rpc jsonRPCRequest
	if err := json.Unmarshal(cfg.Body, &rpc); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if rpc.Method != "getBlockHeight" {
		t.Fatalf("RPC method: got %q, want getBlockHeight", rpc.Method)
	}
	assertCommitment(t, rpc.Params, "finalized")
}

func TestFactory_HeightCheck_GetBlockHeightConfirmed(t *testing.T) {
	t.Parallel()
	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "solana", Type: "solana",
		Mode:        "confirmed",
		HeightCheck: &adapter.HeightCheckOverride{Protocol: "getBlockHeight"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var rpc jsonRPCRequest
	if err := json.Unmarshal(cfg.Body, &rpc); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if rpc.Method != "getBlockHeight" {
		t.Fatalf("RPC method: got %q", rpc.Method)
	}
	assertCommitment(t, rpc.Params, "confirmed")
}

func TestFactory_HeightCheck_UnsupportedMethod(t *testing.T) {
	t.Parallel()
	f := New()
	_, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "solana", Type: "solana",
		HeightCheck: &adapter.HeightCheckOverride{Protocol: "getVersion"},
	})
	if err == nil {
		t.Fatal("expected error for unsupported method")
	}
}

func TestFactory_HeightCheck_AllModesHaveIntegerFormat(t *testing.T) {
	t.Parallel()
	f := New()
	for _, mode := range []string{"", "processed", "confirmed", "finalized"} {
		cfg, err := f.HeightCheck(adapter.NetworkConfig{
			Name: "solana", Type: "solana", Mode: mode,
		})
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if cfg.ResponseFormat != "integer" {
			t.Fatalf("mode %q: ResponseFormat got %q, want integer", mode, cfg.ResponseFormat)
		}
	}
}

// --- HealthChecks tests ---

func TestFactory_HealthChecks_JsonRPC(t *testing.T) {
	t.Parallel()
	f := New()
	checks, err := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Endpoint: map[string]string{"jsonrpc": "http://solana-1:8899"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(checks))
	}
	hc := checks[0]
	if hc.Protocol != "jsonrpc" {
		t.Fatalf("Protocol: got %q", hc.Protocol)
	}
	if hc.Method != "POST" {
		t.Fatalf("Method: got %q", hc.Method)
	}

	var rpc jsonRPCRequest
	if err := json.Unmarshal(hc.Body, &rpc); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if rpc.Method != "getHealth" {
		t.Fatalf("health check method: got %q, want getHealth", rpc.Method)
	}
}

func TestFactory_HealthChecks_WebSocket(t *testing.T) {
	t.Parallel()
	f := New()
	checks, err := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Endpoint: map[string]string{"websocket": "ws://solana-1:8900"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("expected 1, got %d", len(checks))
	}
	if !checks[0].IsWebSocket {
		t.Fatal("expected IsWebSocket=true")
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
		Endpoint: map[string]string{"jsonrpc": "", "websocket": ""},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("expected 0 checks for empty URLs, got %d", len(checks))
	}
}

func TestFactory_HealthChecks_UnknownIgnored(t *testing.T) {
	t.Parallel()
	f := New()
	checks, err := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Endpoint: map[string]string{"grpc": "solana-1:9090"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("expected 0 (grpc ignored), got %d", len(checks))
	}
}

// --- Integration: Factory → Engine ---

func TestIntegration_FactoryToEngine_GetSlot(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":300000000}`))
	}))
	defer srv.Close()

	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{Name: "solana", Type: "solana"})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}

	engine := adapter.NewEngine(srv.Client())
	height, err := engine.CheckHeight(context.Background(), cfg, srv.URL)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if height != 300000000 {
		t.Fatalf("got %d, want 300000000", height)
	}
}

func TestIntegration_FactoryToEngine_GetBlockHeight(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":275000000}`))
	}))
	defer srv.Close()

	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "solana", Type: "solana",
		HeightCheck: &adapter.HeightCheckOverride{Protocol: "getBlockHeight"},
	})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}

	engine := adapter.NewEngine(srv.Client())
	height, err := engine.CheckHeight(context.Background(), cfg, srv.URL)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if height != 275000000 {
		t.Fatalf("got %d, want 275000000", height)
	}
}

func TestIntegration_FactoryToEngine_Error(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"Node is behind"}}`))
	}))
	defer srv.Close()

	f := New()
	cfg, _ := f.HeightCheck(adapter.NetworkConfig{Name: "solana", Type: "solana"})

	engine := adapter.NewEngine(srv.Client())
	_, err := engine.CheckHeight(context.Background(), cfg, srv.URL)
	if err == nil {
		t.Fatal("expected error for JSON-RPC error response")
	}
}

func TestIntegration_HealthCheck_GetHealth(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"ok"}`))
	}))
	defer srv.Close()

	f := New()
	checks, _ := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Endpoint: map[string]string{"jsonrpc": srv.URL},
	})
	if len(checks) == 0 {
		t.Fatal("expected health check config")
	}

	engine := adapter.NewEngine(srv.Client())
	err := engine.CheckHealth(context.Background(), checks[0], srv.URL)
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
}

func TestFactory_ArchivalCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		net              adapter.NetworkConfig
		minHeight        int64
		wantCommitment   string
		wantSlot         float64
	}{
		{
			name:           "default commitment is finalized",
			net:            adapter.NetworkConfig{Name: "solana", Type: "solana"},
			minHeight:      1000,
			wantCommitment: "finalized",
			wantSlot:       1000,
		},
		{
			name: "confirmed commitment",
			net: adapter.NetworkConfig{
				Name: "solana", Type: "solana", Mode: "confirmed",
			},
			minHeight:      500000,
			wantCommitment: "confirmed",
			wantSlot:       500000,
		},
		{
			name: "processed commitment",
			net: adapter.NetworkConfig{
				Name: "solana", Type: "solana", Mode: "processed",
			},
			minHeight:      1,
			wantCommitment: "processed",
			wantSlot:       1,
		},
		{
			name:           "height 0",
			net:            adapter.NetworkConfig{Name: "solana", Type: "solana"},
			minHeight:      0,
			wantCommitment: "finalized",
			wantSlot:       0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := New()
			cfg, err := f.ArchivalCheck(tt.net, tt.minHeight)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if cfg.Method != "POST" {
				t.Fatalf("Method: got %q, want POST", cfg.Method)
			}
			if cfg.URLPath != "/" {
				t.Fatalf("URLPath: got %q, want /", cfg.URLPath)
			}
			if cfg.Headers.Get("Content-Type") != "application/json" {
				t.Fatal("Content-Type header missing or wrong")
			}
			if cfg.ResponsePath != ".result.blockHeight" {
				t.Fatalf("ResponsePath: got %q, want .result.blockHeight", cfg.ResponsePath)
			}
			if cfg.ResponseFormat != "integer" {
				t.Fatalf("ResponseFormat: got %q, want integer", cfg.ResponseFormat)
			}
			if cfg.Protocol != "jsonrpc" {
				t.Fatalf("Protocol: got %q, want jsonrpc", cfg.Protocol)
			}

			// Verify body is valid JSON-RPC with getBlock method.
			var rpc struct {
				JSONRPC string `json:"jsonrpc"`
				Method  string `json:"method"`
				Params  []any  `json:"params"`
				ID      int    `json:"id"`
			}
			if err := json.Unmarshal(cfg.Body, &rpc); err != nil {
				t.Fatalf("body is not valid JSON: %v", err)
			}
			if rpc.Method != "getBlock" {
				t.Fatalf("RPC method: got %q, want getBlock", rpc.Method)
			}
			if len(rpc.Params) < 2 {
				t.Fatalf("expected at least 2 params, got %d", len(rpc.Params))
			}

			// First param is the slot number (integer).
			slot, ok := rpc.Params[0].(float64)
			if !ok {
				t.Fatalf("first param (slot) not number: %T", rpc.Params[0])
			}
			if slot != tt.wantSlot {
				t.Fatalf("slot: got %v, want %v", slot, tt.wantSlot)
			}

			// Second param is the config object with commitment.
			opts, ok := rpc.Params[1].(map[string]any)
			if !ok {
				t.Fatalf("second param not object: %T", rpc.Params[1])
			}
			commitment, ok := opts["commitment"].(string)
			if !ok || commitment != tt.wantCommitment {
				t.Fatalf("commitment: got %q, want %q", opts["commitment"], tt.wantCommitment)
			}
		})
	}
}

// Verify Factory satisfies ChainAdapter interface.
var _ adapter.ChainAdapter = (*Factory)(nil)

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
	ID      int    `json:"id"`
}

func assertCommitment(t *testing.T, params []any, want string) {
	t.Helper()
	if len(params) < 1 {
		t.Fatal("expected at least 1 param")
	}
	obj, ok := params[0].(map[string]any)
	if !ok {
		t.Fatalf("first param not object: %T", params[0])
	}
	got, ok := obj["commitment"].(string)
	if !ok || got != want {
		t.Fatalf("commitment: got %q, want %q", got, want)
	}
}
