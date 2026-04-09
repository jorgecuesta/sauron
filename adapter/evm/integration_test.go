package evm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"sauron/adapter"
)

// TestIntegration_FactoryToEngine tests the full path: EVM factory produces
// a CheckConfig, the engine executes it against a mock EVM node, and the
// hex height is correctly parsed.
func TestIntegration_FactoryToEngine_Latest(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1312d00"}`))
	}))
	defer srv.Close()

	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{Name: "ethereum", Type: "evm"})
	if err != nil {
		t.Fatalf("factory error: %v", err)
	}

	engine := adapter.NewEngine(srv.Client())
	height, err := engine.CheckHeight(context.Background(), cfg, srv.URL)
	if err != nil {
		t.Fatalf("engine error: %v", err)
	}

	if height != 20000000 {
		t.Fatalf("got height %d, want 20000000", height)
	}
}

func TestIntegration_FactoryToEngine_Safe(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		// eth_getBlockByNumber response with block object.
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x1312cf0","hash":"0xabc"}}`))
	}))
	defer srv.Close()

	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "ethereum", Type: "evm", Mode: "safe",
	})
	if err != nil {
		t.Fatalf("factory error: %v", err)
	}

	engine := adapter.NewEngine(srv.Client())
	height, err := engine.CheckHeight(context.Background(), cfg, srv.URL)
	if err != nil {
		t.Fatalf("engine error: %v", err)
	}

	// 0x1312cf0 = 19999984
	if height != 19999984 {
		t.Fatalf("got height %d, want 19999984", height)
	}
}

func TestIntegration_FactoryToEngine_Finalized(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x1312ce0","hash":"0xdef"}}`))
	}))
	defer srv.Close()

	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "ethereum", Type: "evm", Mode: "finalized",
	})
	if err != nil {
		t.Fatalf("factory error: %v", err)
	}

	engine := adapter.NewEngine(srv.Client())
	height, err := engine.CheckHeight(context.Background(), cfg, srv.URL)
	if err != nil {
		t.Fatalf("engine error: %v", err)
	}

	// 0x1312ce0 = 19999968
	if height != 19999968 {
		t.Fatalf("got height %d, want 19999968", height)
	}
}

func TestIntegration_FactoryToEngine_NodeReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"server error"}}`))
	}))
	defer srv.Close()

	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{Name: "ethereum", Type: "evm"})
	if err != nil {
		t.Fatalf("factory error: %v", err)
	}

	engine := adapter.NewEngine(srv.Client())
	_, err = engine.CheckHeight(context.Background(), cfg, srv.URL)
	if err == nil {
		t.Fatal("expected error for JSON-RPC error response")
	}
}

func TestIntegration_FactoryToEngine_NullResult(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
	}))
	defer srv.Close()

	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "ethereum", Type: "evm", Mode: "safe",
	})
	if err != nil {
		t.Fatalf("factory error: %v", err)
	}

	engine := adapter.NewEngine(srv.Client())
	_, err = engine.CheckHeight(context.Background(), cfg, srv.URL)
	if err == nil {
		t.Fatal("expected error for null result (block not found)")
	}
}

func TestIntegration_HealthCheck_NetVersion(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"1"}`))
	}))
	defer srv.Close()

	f := New()
	checks, err := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Endpoint: map[string]string{"jsonrpc": srv.URL},
	})
	if err != nil || len(checks) == 0 {
		t.Fatalf("health check config error: %v", err)
	}

	engine := adapter.NewEngine(srv.Client())
	err = engine.CheckHealth(context.Background(), checks[0], srv.URL)
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
}
