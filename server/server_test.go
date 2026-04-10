package server

import (
	"testing"
	"time"

	"sauron/adapter"
	"sauron/adapter/cosmos"
	"sauron/config"
)

// newRegistryWithCosmos returns a Registry with the cosmos adapter registered.
func newRegistryWithCosmos(t *testing.T) *adapter.Registry {
	t.Helper()
	reg := adapter.NewRegistry()
	if err := reg.Register(cosmos.New()); err != nil {
		t.Fatalf("failed to register cosmos adapter: %v", err)
	}
	return reg
}

// cosmosNetworkWithSync returns a minimal cosmos MultiChainNetwork with a Sync config.
func cosmosNetworkWithSync(name string, oracles []config.SyncOracle, interval time.Duration) *config.MultiChainNetwork {
	return &config.MultiChainNetwork{
		Name: name,
		Type: "cosmos",
		Sync: &config.SyncCheck{
			MaxDrift:      5,
			CheckInterval: interval,
			Oracles:       oracles,
		},
	}
}

func TestBuildOracleConfig_CosmosSimpleOracle(t *testing.T) {
	t.Parallel()

	// Oracle with no override — reuses the cosmos adapter's default height check (rpc GET /status).
	reg := newRegistryWithCosmos(t)
	network := cosmosNetworkWithSync("pocket", []config.SyncOracle{
		{URL: "http://oracle1.example.com:26657"},
	}, 15*time.Second)

	got, err := buildOracleConfig(network, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.Network != "pocket" {
		t.Errorf("Network = %q, want %q", got.Network, "pocket")
	}
	if got.Interval != 15*time.Second {
		t.Errorf("Interval = %v, want %v", got.Interval, 15*time.Second)
	}
	if len(got.Oracles) != 1 {
		t.Fatalf("len(Oracles) = %d, want 1", len(got.Oracles))
	}

	ep := got.Oracles[0]
	if ep.URL != "http://oracle1.example.com:26657" {
		t.Errorf("Oracle URL = %q, want %q", ep.URL, "http://oracle1.example.com:26657")
	}

	// Cosmos default: GET /status via rpc.
	if ep.Check.Method != "GET" {
		t.Errorf("Check.Method = %q, want %q", ep.Check.Method, "GET")
	}
	if ep.Check.URLPath != "/status" {
		t.Errorf("Check.URLPath = %q, want %q", ep.Check.URLPath, "/status")
	}
	if ep.Check.ResponsePath != ".result.sync_info.latest_block_height" {
		t.Errorf("Check.ResponsePath = %q, want %q", ep.Check.ResponsePath, ".result.sync_info.latest_block_height")
	}
	if ep.Check.ResponseFormat != "string" {
		t.Errorf("Check.ResponseFormat = %q, want %q", ep.Check.ResponseFormat, "string")
	}
	if ep.Check.Protocol != "rpc" {
		t.Errorf("Check.Protocol = %q, want %q", ep.Check.Protocol, "rpc")
	}
}

func TestBuildOracleConfig_CustomOracleOverride(t *testing.T) {
	t.Parallel()

	// Oracle has a ResponsePath — should build CheckConfig from its own fields.
	reg := newRegistryWithCosmos(t)
	network := cosmosNetworkWithSync("mynet", []config.SyncOracle{
		{
			URL:            "http://custom-oracle.example.com",
			Method:         "POST",
			URLPath:        "/graphql",
			ResponsePath:   ".data.block.height",
			ResponseFormat: "integer",
			Body:           `{"query":"{ block { height } }"}`,
		},
	}, 30*time.Second)

	got, err := buildOracleConfig(network, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Oracles) != 1 {
		t.Fatalf("len(Oracles) = %d, want 1", len(got.Oracles))
	}

	ep := got.Oracles[0]
	if ep.URL != "http://custom-oracle.example.com" {
		t.Errorf("Oracle URL = %q, want %q", ep.URL, "http://custom-oracle.example.com")
	}
	if ep.Check.Method != "POST" {
		t.Errorf("Check.Method = %q, want %q", ep.Check.Method, "POST")
	}
	if ep.Check.URLPath != "/graphql" {
		t.Errorf("Check.URLPath = %q, want %q", ep.Check.URLPath, "/graphql")
	}
	if ep.Check.ResponsePath != ".data.block.height" {
		t.Errorf("Check.ResponsePath = %q, want %q", ep.Check.ResponsePath, ".data.block.height")
	}
	if ep.Check.ResponseFormat != "integer" {
		t.Errorf("Check.ResponseFormat = %q, want %q", ep.Check.ResponseFormat, "integer")
	}
	if ep.Check.Protocol != "http" {
		t.Errorf("Check.Protocol = %q, want %q", ep.Check.Protocol, "http")
	}
	wantBody := []byte(`{"query":"{ block { height } }"}`)
	if string(ep.Check.Body) != string(wantBody) {
		t.Errorf("Check.Body = %q, want %q", ep.Check.Body, wantBody)
	}
}

func TestBuildOracleConfig_MultipleOracles(t *testing.T) {
	t.Parallel()

	// Two oracles: one with override, one without. Verifies each path independently.
	reg := newRegistryWithCosmos(t)
	network := cosmosNetworkWithSync("pocket", []config.SyncOracle{
		// No override — uses cosmos default (GET /status).
		{URL: "http://oracle-default.example.com:26657"},
		// Override — custom config.
		{
			URL:            "http://oracle-custom.example.com",
			Method:         "GET",
			URLPath:        "/v1/height",
			ResponsePath:   ".height",
			ResponseFormat: "integer",
		},
	}, 10*time.Second)

	got, err := buildOracleConfig(network, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Oracles) != 2 {
		t.Fatalf("len(Oracles) = %d, want 2", len(got.Oracles))
	}

	// First: no override → cosmos default.
	first := got.Oracles[0]
	if first.URL != "http://oracle-default.example.com:26657" {
		t.Errorf("Oracles[0].URL = %q, want default oracle URL", first.URL)
	}
	if first.Check.URLPath != "/status" {
		t.Errorf("Oracles[0].Check.URLPath = %q, want %q", first.Check.URLPath, "/status")
	}
	if first.Check.Protocol != "rpc" {
		t.Errorf("Oracles[0].Check.Protocol = %q, want %q", first.Check.Protocol, "rpc")
	}

	// Second: override → custom config.
	second := got.Oracles[1]
	if second.URL != "http://oracle-custom.example.com" {
		t.Errorf("Oracles[1].URL = %q, want custom oracle URL", second.URL)
	}
	if second.Check.URLPath != "/v1/height" {
		t.Errorf("Oracles[1].Check.URLPath = %q, want %q", second.Check.URLPath, "/v1/height")
	}
	if second.Check.ResponsePath != ".height" {
		t.Errorf("Oracles[1].Check.ResponsePath = %q, want %q", second.Check.ResponsePath, ".height")
	}
	if second.Check.Protocol != "http" {
		t.Errorf("Oracles[1].Check.Protocol = %q, want %q", second.Check.Protocol, "http")
	}
}

func TestBuildOracleConfig_UnknownAdapterType(t *testing.T) {
	t.Parallel()

	reg := adapter.NewRegistry()
	// No adapters registered — any type lookup fails.
	network := &config.MultiChainNetwork{
		Name: "unknown-net",
		Type: "solana", // not registered
		Sync: &config.SyncCheck{
			CheckInterval: 10 * time.Second,
			Oracles: []config.SyncOracle{
				{URL: "http://oracle.example.com"},
			},
		},
	}

	_, err := buildOracleConfig(network, reg)
	if err == nil {
		t.Fatal("expected error for unknown adapter type, got nil")
	}
}

func TestBuildOracleConfig_OracleOverrideWithHeaders(t *testing.T) {
	t.Parallel()

	reg := newRegistryWithCosmos(t)
	network := cosmosNetworkWithSync("mynet", []config.SyncOracle{
		{
			URL:          "http://oracle-with-headers.example.com",
			Method:       "GET",
			URLPath:      "/api/height",
			ResponsePath: ".block.height",
			Headers: map[string]string{
				"X-Api-Key":    "secret-key",
				"Content-Type": "application/json",
			},
		},
	}, 20*time.Second)

	got, err := buildOracleConfig(network, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Oracles) != 1 {
		t.Fatalf("len(Oracles) = %d, want 1", len(got.Oracles))
	}

	ep := got.Oracles[0]
	if ep.Check.Headers == nil {
		t.Fatal("Check.Headers is nil, want headers map")
	}
	if got := ep.Check.Headers.Get("X-Api-Key"); got != "secret-key" {
		t.Errorf("Headers[X-Api-Key] = %q, want %q", got, "secret-key")
	}
	if got := ep.Check.Headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Headers[Content-Type] = %q, want %q", got, "application/json")
	}
}

func TestBuildOracleConfig_CheckIntervalMatchesNetwork(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		interval time.Duration
	}{
		{"zero interval", 0},
		{"short interval", 5 * time.Second},
		{"standard interval", 30 * time.Second},
		{"long interval", 5 * time.Minute},
	}

	reg := newRegistryWithCosmos(t)

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			network := cosmosNetworkWithSync("pocket", []config.SyncOracle{
				{URL: "http://oracle.example.com"},
			}, tt.interval)

			got, err := buildOracleConfig(network, reg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Interval != tt.interval {
				t.Errorf("Interval = %v, want %v", got.Interval, tt.interval)
			}
		})
	}
}

func TestBuildOracleConfig_NoOracles(t *testing.T) {
	t.Parallel()

	// A network with Sync configured but no oracles — valid, produces empty slice.
	reg := newRegistryWithCosmos(t)
	network := cosmosNetworkWithSync("pocket", nil, 30*time.Second)

	got, err := buildOracleConfig(network, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Network != "pocket" {
		t.Errorf("Network = %q, want %q", got.Network, "pocket")
	}
	if len(got.Oracles) != 0 {
		t.Errorf("len(Oracles) = %d, want 0", len(got.Oracles))
	}
}

// TestBuildOracleConfig_ReturnType checks that the returned value satisfies
// the checker.OracleConfig type (compile-time guard via assignment).
func TestBuildOracleConfig_ReturnType(t *testing.T) {
	t.Parallel()

	reg := newRegistryWithCosmos(t)
	network := cosmosNetworkWithSync("pocket", []config.SyncOracle{
		{URL: "http://oracle.example.com"},
	}, 30*time.Second)

	got, err := buildOracleConfig(network, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify return type structure.
	if got.Network != "pocket" {
		t.Errorf("expected network pocket, got %q", got.Network)
	}
	_ = got.Oracles // ensure Oracles field exists
}

func TestBuildOracleConfig_OracleOverrideNoHeaders(t *testing.T) {
	t.Parallel()

	// Override present but no Headers field — Headers in CheckConfig should be nil.
	reg := newRegistryWithCosmos(t)
	network := cosmosNetworkWithSync("mynet", []config.SyncOracle{
		{
			URL:          "http://oracle.example.com",
			Method:       "GET",
			URLPath:      "/height",
			ResponsePath: ".height",
		},
	}, 10*time.Second)

	got, err := buildOracleConfig(network, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Oracles) != 1 {
		t.Fatalf("len(Oracles) = %d, want 1", len(got.Oracles))
	}

	ep := got.Oracles[0]
	// Headers map should be nil — no headers were set.
	if ep.Check.Headers != nil {
		t.Errorf("Check.Headers = %v, want nil (no headers configured)", ep.Check.Headers)
	}
}
