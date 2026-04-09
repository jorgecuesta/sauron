package evm

import (
	"encoding/json"
	"net/http"
	"testing"

	"sauron/adapter"
)

func TestFactory_Type(t *testing.T) {
	t.Parallel()
	f := New()
	if got := f.Type(); got != "evm" {
		t.Fatalf("got %q, want %q", got, "evm")
	}
}

// --- HeightCheck tests ---

func TestFactory_HeightCheck_DefaultLatest(t *testing.T) {
	t.Parallel()
	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{Name: "ethereum", Type: "evm"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Method != "POST" {
		t.Fatalf("Method: got %q, want POST", cfg.Method)
	}
	if cfg.URLPath != "/" {
		t.Fatalf("URLPath: got %q, want /", cfg.URLPath)
	}
	if cfg.ResponsePath != ".result" {
		t.Fatalf("ResponsePath: got %q, want .result", cfg.ResponsePath)
	}
	if cfg.ResponseFormat != "hex" {
		t.Fatalf("ResponseFormat: got %q, want hex", cfg.ResponseFormat)
	}
	if cfg.Protocol != "jsonrpc" {
		t.Fatalf("Protocol: got %q, want jsonrpc", cfg.Protocol)
	}
	if cfg.Headers.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type header missing")
	}

	// Verify body is valid JSON-RPC for eth_blockNumber.
	var rpc jsonRPCRequest
	if err := json.Unmarshal(cfg.Body, &rpc); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if rpc.Method != "eth_blockNumber" {
		t.Fatalf("RPC method: got %q, want eth_blockNumber", rpc.Method)
	}
}

func TestFactory_HeightCheck_ExplicitLatest(t *testing.T) {
	t.Parallel()
	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "ethereum",
		Type: "evm",
		Mode: "latest",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ResponsePath != ".result" {
		t.Fatalf("ResponsePath: got %q, want .result", cfg.ResponsePath)
	}

	var rpc jsonRPCRequest
	if err := json.Unmarshal(cfg.Body, &rpc); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if rpc.Method != "eth_blockNumber" {
		t.Fatalf("RPC method: got %q, want eth_blockNumber", rpc.Method)
	}
}

func TestFactory_HeightCheck_Safe(t *testing.T) {
	t.Parallel()
	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "ethereum",
		Type: "evm",
		Mode: "safe",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Safe mode uses eth_getBlockByNumber with "safe" tag.
	if cfg.ResponsePath != ".result.number" {
		t.Fatalf("ResponsePath: got %q, want .result.number", cfg.ResponsePath)
	}

	var rpc jsonRPCRequest
	if err := json.Unmarshal(cfg.Body, &rpc); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if rpc.Method != "eth_getBlockByNumber" {
		t.Fatalf("RPC method: got %q, want eth_getBlockByNumber", rpc.Method)
	}
	if len(rpc.Params) < 1 {
		t.Fatal("expected at least 1 param")
	}
	tag, ok := rpc.Params[0].(string)
	if !ok || tag != "safe" {
		t.Fatalf("first param: got %v, want \"safe\"", rpc.Params[0])
	}
}

func TestFactory_HeightCheck_Finalized(t *testing.T) {
	t.Parallel()
	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "ethereum",
		Type: "evm",
		Mode: "finalized",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.ResponsePath != ".result.number" {
		t.Fatalf("ResponsePath: got %q, want .result.number", cfg.ResponsePath)
	}

	var rpc jsonRPCRequest
	if err := json.Unmarshal(cfg.Body, &rpc); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if rpc.Method != "eth_getBlockByNumber" {
		t.Fatalf("RPC method: got %q, want eth_getBlockByNumber", rpc.Method)
	}
	if len(rpc.Params) < 1 {
		t.Fatal("expected at least 1 param")
	}
	tag, ok := rpc.Params[0].(string)
	if !ok || tag != "finalized" {
		t.Fatalf("first param: got %v, want \"finalized\"", rpc.Params[0])
	}
}

func TestFactory_HeightCheck_UnsupportedMode(t *testing.T) {
	t.Parallel()
	f := New()
	_, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "ethereum",
		Type: "evm",
		Mode: "pending",
	})
	if err == nil {
		t.Fatal("expected error for unsupported mode")
	}
}

func TestFactory_HeightCheck_AllModesHaveHexFormat(t *testing.T) {
	t.Parallel()
	f := New()

	for _, mode := range []string{"", "latest", "safe", "finalized"} {
		cfg, err := f.HeightCheck(adapter.NetworkConfig{
			Name: "ethereum",
			Type: "evm",
			Mode: mode,
		})
		if err != nil {
			t.Fatalf("mode %q: unexpected error: %v", mode, err)
		}
		if cfg.ResponseFormat != "hex" {
			t.Fatalf("mode %q: ResponseFormat got %q, want hex", mode, cfg.ResponseFormat)
		}
	}
}

// --- HealthChecks tests ---

func TestFactory_HealthChecks_JsonRPC(t *testing.T) {
	t.Parallel()
	f := New()
	checks, err := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Name:    "geth-1",
		Network: "ethereum",
		Endpoint: map[string]string{
			"jsonrpc": "http://geth-1:8545",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(checks))
	}

	hc := checks[0]
	if hc.Protocol != "jsonrpc" {
		t.Fatalf("Protocol: got %q, want jsonrpc", hc.Protocol)
	}
	if hc.Method != "POST" {
		t.Fatalf("Method: got %q, want POST", hc.Method)
	}
	if hc.ExpectStatus != http.StatusOK {
		t.Fatalf("ExpectStatus: got %d, want 200", hc.ExpectStatus)
	}
	if hc.Headers.Get("Content-Type") != "application/json" {
		t.Fatal("expected Content-Type header")
	}
	if len(hc.Body) == 0 {
		t.Fatal("expected non-empty body for net_version check")
	}

	var rpc jsonRPCRequest
	if err := json.Unmarshal(hc.Body, &rpc); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if rpc.Method != "net_version" {
		t.Fatalf("health check method: got %q, want net_version", rpc.Method)
	}
}

func TestFactory_HealthChecks_WebSocket(t *testing.T) {
	t.Parallel()
	f := New()
	checks, err := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Name:    "geth-1",
		Network: "ethereum",
		Endpoint: map[string]string{
			"websocket": "ws://geth-1:8546",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(checks))
	}
	if !checks[0].IsWebSocket {
		t.Fatal("expected IsWebSocket=true")
	}
	if checks[0].Protocol != "websocket" {
		t.Fatalf("Protocol: got %q, want websocket", checks[0].Protocol)
	}
}

func TestFactory_HealthChecks_AllEndpoints(t *testing.T) {
	t.Parallel()
	f := New()
	checks, err := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Name:    "geth-1",
		Network: "ethereum",
		Endpoint: map[string]string{
			"jsonrpc":   "http://geth-1:8545",
			"websocket": "ws://geth-1:8546",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("expected 2 checks, got %d", len(checks))
	}

	protos := make(map[string]bool)
	for _, c := range checks {
		protos[c.Protocol] = true
	}
	if !protos["jsonrpc"] || !protos["websocket"] {
		t.Fatalf("expected jsonrpc and websocket, got %v", protos)
	}
}

func TestFactory_HealthChecks_EmptyEndpoints(t *testing.T) {
	t.Parallel()
	f := New()
	checks, err := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Name:     "geth-1",
		Network:  "ethereum",
		Endpoint: map[string]string{},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("expected 0 checks, got %d", len(checks))
	}
}

func TestFactory_HealthChecks_EmptyURLSkipped(t *testing.T) {
	t.Parallel()
	f := New()
	checks, err := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Name:    "geth-1",
		Network: "ethereum",
		Endpoint: map[string]string{
			"jsonrpc": "",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("expected 0 checks (empty URL), got %d", len(checks))
	}
}

func TestFactory_HealthChecks_UnknownProtocolIgnored(t *testing.T) {
	t.Parallel()
	f := New()
	checks, err := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Name:    "geth-1",
		Network: "ethereum",
		Endpoint: map[string]string{
			"grpc":    "geth-1:9090", // EVM doesn't use gRPC
			"jsonrpc": "http://geth-1:8545",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("expected 1 check (grpc ignored), got %d", len(checks))
	}
	if checks[0].Protocol != "jsonrpc" {
		t.Fatalf("expected jsonrpc, got %q", checks[0].Protocol)
	}
}

// Verify Factory satisfies ChainAdapter interface.
var _ adapter.ChainAdapter = (*Factory)(nil)

// jsonRPCRequest is used for JSON-RPC body verification in tests.
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
	ID      int    `json:"id"`
}
