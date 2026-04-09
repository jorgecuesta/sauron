package cosmos

import (
	"testing"

	"sauron/adapter"
)

func TestFactory_Type(t *testing.T) {
	t.Parallel()
	f := New()
	if got := f.Type(); got != "cosmos" {
		t.Fatalf("got %q, want %q", got, "cosmos")
	}
}

func TestFactory_HeightCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		net      adapter.NetworkConfig
		wantPath string
		wantURL  string
		isGRPC   bool
		wantErr  bool
	}{
		{
			name:     "default protocol is rpc",
			net:      adapter.NetworkConfig{Name: "pocket", Type: "cosmos"},
			wantPath: ".result.sync_info.latest_block_height",
			wantURL:  "/status",
		},
		{
			name: "explicit rpc",
			net: adapter.NetworkConfig{
				Name: "pocket",
				Type: "cosmos",
				HeightCheck: &adapter.HeightCheckOverride{
					Protocol: "rpc",
				},
			},
			wantPath: ".result.sync_info.latest_block_height",
			wantURL:  "/status",
		},
		{
			name: "rest protocol",
			net: adapter.NetworkConfig{
				Name: "pocket",
				Type: "cosmos",
				HeightCheck: &adapter.HeightCheckOverride{
					Protocol: "rest",
				},
			},
			wantPath: ".sdk_block.header.height",
			wantURL:  "/cosmos/base/tendermint/v1beta1/blocks/latest",
		},
		{
			name: "grpc protocol",
			net: adapter.NetworkConfig{
				Name: "pocket",
				Type: "cosmos",
				HeightCheck: &adapter.HeightCheckOverride{
					Protocol: "grpc",
				},
			},
			isGRPC: true,
		},
		{
			name: "unsupported protocol",
			net: adapter.NetworkConfig{
				Name: "pocket",
				Type: "cosmos",
				HeightCheck: &adapter.HeightCheckOverride{
					Protocol: "websocket",
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := New()
			cfg, err := f.HeightCheck(tt.net)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.isGRPC {
				if !cfg.IsGRPC {
					t.Fatal("expected IsGRPC=true")
				}
				if cfg.GRPCService == "" {
					t.Fatal("expected GRPCService to be set")
				}
				return
			}

			if cfg.ResponsePath != tt.wantPath {
				t.Fatalf("ResponsePath: got %q, want %q", cfg.ResponsePath, tt.wantPath)
			}
			if cfg.URLPath != tt.wantURL {
				t.Fatalf("URLPath: got %q, want %q", cfg.URLPath, tt.wantURL)
			}
			if cfg.Method != "GET" {
				t.Fatalf("Method: got %q, want GET", cfg.Method)
			}
			if cfg.ResponseFormat != "string" {
				t.Fatalf("ResponseFormat: got %q, want string", cfg.ResponseFormat)
			}
		})
	}
}

func TestFactory_HealthChecks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		node       adapter.NodeConfig
		wantCount  int
		wantProtos []string
	}{
		{
			name: "all protocols",
			node: adapter.NodeConfig{
				Name:    "seed-one",
				Network: "pocket",
				Endpoint: map[string]string{
					"rest": "http://seed-one:1317",
					"rpc":  "http://seed-one:26657",
					"grpc": "seed-one:9090",
				},
			},
			wantCount:  4, // rest + rpc + websocket + grpc
			wantProtos: []string{"rest", "rpc", "websocket", "grpc"},
		},
		{
			name: "rpc only",
			node: adapter.NodeConfig{
				Name:    "seed-one",
				Network: "pocket",
				Endpoint: map[string]string{
					"rpc": "http://seed-one:26657",
				},
			},
			wantCount:  2, // rpc + websocket
			wantProtos: []string{"rpc", "websocket"},
		},
		{
			name: "rest only",
			node: adapter.NodeConfig{
				Name:    "seed-one",
				Network: "pocket",
				Endpoint: map[string]string{
					"rest": "http://seed-one:1317",
				},
			},
			wantCount:  1,
			wantProtos: []string{"rest"},
		},
		{
			name: "grpc only",
			node: adapter.NodeConfig{
				Name:    "seed-one",
				Network: "pocket",
				Endpoint: map[string]string{
					"grpc": "seed-one:9090",
				},
			},
			wantCount:  1,
			wantProtos: []string{"grpc"},
		},
		{
			name: "empty endpoints",
			node: adapter.NodeConfig{
				Name:     "seed-one",
				Network:  "pocket",
				Endpoint: map[string]string{},
			},
			wantCount: 0,
		},
		{
			name: "empty url skipped",
			node: adapter.NodeConfig{
				Name:    "seed-one",
				Network: "pocket",
				Endpoint: map[string]string{
					"rest": "",
					"rpc":  "http://seed-one:26657",
				},
			},
			wantCount:  2, // rpc + websocket
			wantProtos: []string{"rpc", "websocket"},
		},
		{
			name: "unknown protocol ignored",
			node: adapter.NodeConfig{
				Name:    "seed-one",
				Network: "pocket",
				Endpoint: map[string]string{
					"jsonrpc": "http://seed-one:8545",
					"rpc":     "http://seed-one:26657",
				},
			},
			wantCount:  2, // only rpc + websocket, jsonrpc is unknown for cosmos
			wantProtos: []string{"rpc", "websocket"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := New()
			checks, err := f.HealthChecks(adapter.NetworkConfig{}, tt.node)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(checks) != tt.wantCount {
				t.Fatalf("got %d checks, want %d", len(checks), tt.wantCount)
			}

			protoSet := make(map[string]bool)
			for _, c := range checks {
				protoSet[c.Protocol] = true
			}
			for _, p := range tt.wantProtos {
				if !protoSet[p] {
					t.Errorf("missing expected protocol %q in health checks", p)
				}
			}
		})
	}
}

// TestFactory_HealthChecks_Fields verifies individual fields on generated health checks.
func TestFactory_HealthChecks_Fields(t *testing.T) {
	t.Parallel()

	f := New()
	checks, err := f.HealthChecks(adapter.NetworkConfig{}, adapter.NodeConfig{
		Name:    "seed-one",
		Network: "pocket",
		Endpoint: map[string]string{
			"rest": "http://seed-one:1317",
			"rpc":  "http://seed-one:26657",
			"grpc": "seed-one:9090",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	byProto := make(map[string]adapter.HealthCheckConfig)
	for _, c := range checks {
		byProto[c.Protocol] = c
	}

	// REST health check.
	rest := byProto["rest"]
	if rest.Method != "GET" {
		t.Errorf("rest Method: got %q, want GET", rest.Method)
	}
	if rest.URLPath != "/cosmos/base/tendermint/v1beta1/blocks/latest" {
		t.Errorf("rest URLPath: got %q", rest.URLPath)
	}
	if rest.ExpectStatus != 200 {
		t.Errorf("rest ExpectStatus: got %d, want 200", rest.ExpectStatus)
	}
	if rest.IsGRPC || rest.IsWebSocket {
		t.Error("rest should not be gRPC or WebSocket")
	}

	// RPC health check.
	rpc := byProto["rpc"]
	if rpc.Method != "GET" {
		t.Errorf("rpc Method: got %q, want GET", rpc.Method)
	}
	if rpc.URLPath != "/status" {
		t.Errorf("rpc URLPath: got %q, want /status", rpc.URLPath)
	}

	// WebSocket health check (auto-generated from RPC).
	ws := byProto["websocket"]
	if !ws.IsWebSocket {
		t.Error("websocket check should have IsWebSocket=true")
	}

	// gRPC health check.
	grpc := byProto["grpc"]
	if !grpc.IsGRPC {
		t.Error("grpc check should have IsGRPC=true")
	}
}

// TestFactory_HeightCheck_GRPCFields verifies gRPC height check has all required fields.
func TestFactory_HeightCheck_GRPCFields(t *testing.T) {
	t.Parallel()

	f := New()
	cfg, err := f.HeightCheck(adapter.NetworkConfig{
		Name: "pocket",
		Type: "cosmos",
		HeightCheck: &adapter.HeightCheckOverride{
			Protocol: "grpc",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.IsGRPC {
		t.Fatal("expected IsGRPC=true")
	}
	if cfg.GRPCService != "cosmos.base.tendermint.v1beta1.Service" {
		t.Fatalf("GRPCService: got %q", cfg.GRPCService)
	}
	if cfg.GRPCMethod != "GetLatestBlock" {
		t.Fatalf("GRPCMethod: got %q, want GetLatestBlock", cfg.GRPCMethod)
	}
	if cfg.Protocol != "grpc" {
		t.Fatalf("Protocol: got %q, want grpc", cfg.Protocol)
	}
}

// Verify Factory satisfies ChainAdapter interface.
var _ adapter.ChainAdapter = (*Factory)(nil)
