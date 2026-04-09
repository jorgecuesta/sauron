package config

import (
	"os"
	"testing"
	"time"

	"go.uber.org/zap"
)

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "multichain-*.yaml")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close config: %v", err)
	}
	return f.Name()
}

const validMultiChainYAML = `
listen: ":3000"
auth: false

timeouts:
  health_check: 5s
  proxy: 60s

networks:
  - name: pocket
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rpc
        listen: ":8081"
      - protocol: rest
        listen: ":8080"

internals:
  - name: seed-one
    network: pocket
    endpoints:
      rpc: "http://seed-one:26657"
      rest: "http://seed-one:1317"
`

func TestMultiChainLoader_LoadValid(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t, validMultiChainYAML)
	loader, err := NewMultiChainLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := loader.Get()
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.Listen != ":3000" {
		t.Fatalf("Listen: got %q", cfg.Listen)
	}
	if len(cfg.Networks) != 1 {
		t.Fatalf("Networks: got %d", len(cfg.Networks))
	}
	if cfg.Networks[0].Name != "pocket" {
		t.Fatalf("Network name: got %q", cfg.Networks[0].Name)
	}
	if cfg.Networks[0].Type != "cosmos" {
		t.Fatalf("Network type: got %q", cfg.Networks[0].Type)
	}
	if len(cfg.Networks[0].Endpoints) != 2 {
		t.Fatalf("Endpoints: got %d", len(cfg.Networks[0].Endpoints))
	}
	if len(cfg.Internals) != 1 {
		t.Fatalf("Internals: got %d", len(cfg.Internals))
	}
	if cfg.Internals[0].Name != "seed-one" {
		t.Fatalf("Node name: got %q", cfg.Internals[0].Name)
	}
}

func TestMultiChainLoader_Defaults(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t, validMultiChainYAML)
	loader, err := NewMultiChainLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := loader.Get()

	// GRPCKeepalive defaults.
	if cfg.GRPCKeepalive.Time != 600*time.Second {
		t.Fatalf("GRPCKeepalive.Time: got %v, want 600s", cfg.GRPCKeepalive.Time)
	}
	if cfg.GRPCKeepalive.Timeout != 20*time.Second {
		t.Fatalf("GRPCKeepalive.Timeout: got %v, want 20s", cfg.GRPCKeepalive.Timeout)
	}

	// ExternalFailoverThreshold default.
	if cfg.ExternalFailoverThreshold != 2 {
		t.Fatalf("ExternalFailoverThreshold: got %d, want 2", cfg.ExternalFailoverThreshold)
	}
}

func TestMultiChainLoader_InvalidConfig(t *testing.T) {
	t.Parallel()

	// Missing listen address.
	path := writeTestConfig(t, `
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: rpc
        listen: ":8081"
internals:
  - name: seed-one
    network: pocket
    endpoints:
      rpc: "http://seed-one:26657"
`)
	_, err := NewMultiChainLoader(path, zap.NewNop())
	if err == nil {
		t.Fatal("expected error for missing listen address")
	}
}

func TestMultiChainLoader_MalformedYAML(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t, `not: valid: yaml: {{{{`)
	_, err := NewMultiChainLoader(path, zap.NewNop())
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}
}

func TestMultiChainLoader_FileNotFound(t *testing.T) {
	t.Parallel()

	_, err := NewMultiChainLoader("/nonexistent/path.yaml", zap.NewNop())
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestMultiChainLoader_GetReturnsImmutablePointer(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t, validMultiChainYAML)
	loader, err := NewMultiChainLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg1 := loader.Get()
	cfg2 := loader.Get()

	// Same pointer — atomic.Pointer returns the same stored pointer.
	if cfg1 != cfg2 {
		t.Fatal("expected same pointer from consecutive Get() calls")
	}
}

func TestMultiChainLoader_MultiNetworkConfig(t *testing.T) {
	t.Parallel()

	yaml := `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    height_check:
      protocol: rpc
    endpoints:
      - protocol: rpc
        listen: ":8081"
  - name: ethereum
    type: evm
    mode: finalized
    endpoints:
      - protocol: jsonrpc
        listen: ":9080"
internals:
  - name: seed-one
    network: pocket
    endpoints:
      rpc: "http://seed-one:26657"
  - name: geth-1
    network: ethereum
    endpoints:
      jsonrpc: "http://geth-1:8545"
`
	path := writeTestConfig(t, yaml)
	loader, err := NewMultiChainLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := loader.Get()
	if len(cfg.Networks) != 2 {
		t.Fatalf("expected 2 networks, got %d", len(cfg.Networks))
	}

	pocket := cfg.FindNetwork("pocket")
	if pocket == nil {
		t.Fatal("pocket network not found")
	}
	if pocket.Type != "cosmos" {
		t.Fatalf("pocket type: got %q", pocket.Type)
	}

	eth := cfg.FindNetwork("ethereum")
	if eth == nil {
		t.Fatal("ethereum network not found")
	}
	if eth.Type != "evm" {
		t.Fatalf("ethereum type: got %q", eth.Type)
	}
	if eth.Mode != "finalized" {
		t.Fatalf("ethereum mode: got %q", eth.Mode)
	}
}

func TestMultiChainLoader_CustomNetworkConfig(t *testing.T) {
	t.Parallel()

	yaml := `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: mina
    type: custom
    height_check:
      method: POST
      url_path: /graphql
      headers:
        Content-Type: application/json
      body: '{"query":"{ bestChain(maxLength:1) { protocolState { consensusState { blockHeight } } } }"}'
      response_path: ".data.bestChain[0].protocolState.consensusState.blockHeight"
      response_format: integer
    endpoints:
      - protocol: http
        listen: ":9100"
internals:
  - name: mina-node-1
    network: mina
    endpoints:
      http: "http://mina-1:3085"
`
	path := writeTestConfig(t, yaml)
	loader, err := NewMultiChainLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := loader.Get()
	mina := cfg.FindNetwork("mina")
	if mina == nil {
		t.Fatal("mina network not found")
	}
	if mina.Type != "custom" {
		t.Fatalf("type: got %q", mina.Type)
	}
	if mina.HeightCheck == nil {
		t.Fatal("expected HeightCheck to be set")
	}
	if mina.HeightCheck.Method != "POST" {
		t.Fatalf("method: got %q", mina.HeightCheck.Method)
	}
	if mina.HeightCheck.ResponsePath != ".data.bestChain[0].protocolState.consensusState.blockHeight" {
		t.Fatalf("response_path: got %q", mina.HeightCheck.ResponsePath)
	}
}

func TestMultiChainLoader_WithSyncOracles(t *testing.T) {
	t.Parallel()

	yaml := `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: ethereum
    type: evm
    endpoints:
      - protocol: jsonrpc
        listen: ":9080"
    sync:
      max_drift: 5
      check_interval: 30s
      oracles:
        - url: "https://rpc.ankr.com/eth"
internals:
  - name: geth-1
    network: ethereum
    endpoints:
      jsonrpc: "http://geth-1:8545"
`
	path := writeTestConfig(t, yaml)
	loader, err := NewMultiChainLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := loader.Get()
	eth := cfg.FindNetwork("ethereum")
	if eth == nil {
		t.Fatal("ethereum not found")
	}
	if eth.Sync == nil {
		t.Fatal("expected Sync to be set")
	}
	if eth.Sync.MaxDrift != 5 {
		t.Fatalf("max_drift: got %d", eth.Sync.MaxDrift)
	}
	if len(eth.Sync.Oracles) != 1 {
		t.Fatalf("oracles: got %d", len(eth.Sync.Oracles))
	}
	if eth.Sync.Oracles[0].URL != "https://rpc.ankr.com/eth" {
		t.Fatalf("oracle URL: got %q", eth.Sync.Oracles[0].URL)
	}
}

func TestMultiChainLoader_WithArchival(t *testing.T) {
	t.Parallel()

	yaml := `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: ethereum
    type: evm
    endpoints:
      - protocol: jsonrpc
        listen: ":9080"
    archival:
      min_height: 1
      check_interval: 5m
internals:
  - name: geth-1
    network: ethereum
    endpoints:
      jsonrpc: "http://geth-1:8545"
`
	path := writeTestConfig(t, yaml)
	loader, err := NewMultiChainLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := loader.Get()
	eth := cfg.FindNetwork("ethereum")
	if eth.Archival == nil {
		t.Fatal("expected Archival to be set")
	}
	if eth.Archival.MinHeight != 1 {
		t.Fatalf("min_height: got %d", eth.Archival.MinHeight)
	}
}

func TestMultiChainLoader_WithUsersAndPermissions(t *testing.T) {
	t.Parallel()

	yaml := `
listen: ":3000"
auth: true
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: rpc
        listen: ":8081"
internals:
  - name: seed-one
    network: pocket
    endpoints:
      rpc: "http://seed-one:26657"
users:
  - name: admin
    token: "admin-token"
    permissions: all
  - name: reader
    token: "reader-token"
    permissions:
      pocket:
        rpc: true
`
	path := writeTestConfig(t, yaml)
	loader, err := NewMultiChainLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := loader.Get()
	if len(cfg.Users) != 2 {
		t.Fatalf("users: got %d", len(cfg.Users))
	}

	admin := cfg.FindUser("admin-token")
	if admin == nil {
		t.Fatal("admin user not found")
	}
	if !admin.Permissions.All {
		t.Fatal("admin should have All permissions")
	}

	reader := cfg.FindUser("reader-token")
	if reader == nil {
		t.Fatal("reader user not found")
	}
	if reader.Permissions.All {
		t.Fatal("reader should NOT have All permissions")
	}
	if !reader.Permissions.HasAccess("pocket", "rpc") {
		t.Fatal("reader should have pocket:rpc access")
	}
	if reader.Permissions.HasAccess("pocket", "rest") {
		t.Fatal("reader should NOT have pocket:rest access")
	}
}
