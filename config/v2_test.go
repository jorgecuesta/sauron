package config

import (
	"testing"
	"time"
)

// validV2Config returns a minimal valid V2Config for testing.
// Tests should modify specific fields to test validation.
func validV2Config() V2Config {
	return V2Config{
		Listen: ":3000",
		Timeouts: Timeouts{
			HealthCheck: 5 * time.Second,
			Proxy:       60 * time.Second,
		},
		Networks: []V2Network{
			{
				Name: "pocket",
				Type: "cosmos",
				HeightCheck: &V2HeightCheck{
					Protocol: "rpc",
					Interval: 30 * time.Second,
				},
				Endpoints: []V2Endpoint{
					{Protocol: "rpc", Listen: ":8081"},
				},
			},
		},
		Internals: []V2Node{
			{
				Name:    "seed-one",
				Network: "pocket",
				Endpoints: map[string]string{
					"rpc": "http://seed-one:26657",
				},
			},
		},
	}
}

func TestValidateV2_ValidMinimal(t *testing.T) {
	t.Parallel()
	cfg := validV2Config()
	if err := ValidateV2(&cfg); err != nil {
		t.Fatalf("expected valid, got error: %v", err)
	}
}

func TestValidateV2_ValidFull(t *testing.T) {
	t.Parallel()
	cfg := V2Config{
		Listen: ":3000",
		Auth:   true,
		Timeouts: Timeouts{
			HealthCheck: 5 * time.Second,
			Proxy:       300 * time.Second,
		},
		GRPCKeepalive: GRPCKeepalive{
			Time:    600 * time.Second,
			Timeout: 20 * time.Second,
		},
		Redis: Redis{
			Enabled: true,
			URI:     "redis://localhost:6379",
		},
		Networks: []V2Network{
			{
				Name: "pocket",
				Type: "cosmos",
				HeightCheck: &V2HeightCheck{
					Protocol: "rpc",
					Interval: 30 * time.Second,
				},
				Endpoints: []V2Endpoint{
					{Protocol: "rest", Listen: ":8080", Advertise: "https://sauron-api.example.com"},
					{Protocol: "rpc", Listen: ":8081", Advertise: "https://sauron-rpc.example.com"},
					{Protocol: "grpc", Listen: ":8082", Advertise: "sauron-grpc.example.com:9090"},
				},
			},
			{
				Name: "ethereum",
				Type: "evm",
				Mode: "finalized",
				Endpoints: []V2Endpoint{
					{Protocol: "jsonrpc", Listen: ":9080"},
				},
				Sync: &V2SyncCheck{
					MaxDrift:      5,
					CheckInterval: 30 * time.Second,
					Oracles:       []V2Oracle{{URL: "https://rpc.ankr.com/eth"}},
				},
			},
		},
		Internals: []V2Node{
			{
				Name:    "seed-one",
				Network: "pocket",
				Endpoints: map[string]string{
					"rest": "http://seed-one:1317",
					"rpc":  "http://seed-one:26657",
					"grpc": "seed-one:9090",
				},
				GRPCInsecure: true,
			},
			{
				Name:    "eth-geth-1",
				Network: "ethereum",
				Endpoints: map[string]string{
					"jsonrpc": "http://geth-1:8545",
				},
			},
		},
		Externals: []External{
			{
				Name:  "us-west",
				Token: "shared-secret",
				Rings: []string{"https://sauron-west:3000"},
			},
		},
		Users: []V2User{
			{
				Name:  "indexer",
				Token: "idx-token",
				Permissions: V2Permissions{
					Networks: map[string]map[string]bool{
						"pocket":   {"rest": true, "rpc": true},
						"ethereum": {"jsonrpc": true},
					},
				},
			},
			{
				Name:        "admin",
				Token:       "admin-token",
				Permissions: V2Permissions{All: true},
			},
		},
	}
	if err := ValidateV2(&cfg); err != nil {
		t.Fatalf("expected valid, got error: %v", err)
	}
}

func TestValidateV2_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		modify func(*V2Config)
		errMsg string
	}{
		// Top-level.
		{
			name:   "empty listen",
			modify: func(c *V2Config) { c.Listen = "" },
			errMsg: "listen address cannot be empty",
		},
		{
			name:   "invalid listen format",
			modify: func(c *V2Config) { c.Listen = "badformat" },
			errMsg: "invalid listen format",
		},

		// Timeouts.
		{
			name:   "zero health check timeout",
			modify: func(c *V2Config) { c.Timeouts.HealthCheck = 0 },
			errMsg: "health_check timeout cannot be zero",
		},
		{
			name:   "short health check timeout",
			modify: func(c *V2Config) { c.Timeouts.HealthCheck = 500 * time.Millisecond },
			errMsg: "health_check timeout too short",
		},
		{
			name:   "zero proxy timeout",
			modify: func(c *V2Config) { c.Timeouts.Proxy = 0 },
			errMsg: "proxy timeout cannot be zero",
		},
		{
			name:   "short proxy timeout",
			modify: func(c *V2Config) { c.Timeouts.Proxy = 100 * time.Millisecond },
			errMsg: "proxy timeout too short",
		},

		// Redis.
		{
			name:   "redis enabled without URI",
			modify: func(c *V2Config) { c.Redis = Redis{Enabled: true} },
			errMsg: "redis URI cannot be empty",
		},
		{
			name:   "redis bad URI",
			modify: func(c *V2Config) { c.Redis = Redis{Enabled: true, URI: "tcp://bad"} },
			errMsg: "invalid redis URI format",
		},

		// Networks.
		{
			name:   "no networks",
			modify: func(c *V2Config) { c.Networks = nil },
			errMsg: "at least one network must be configured",
		},
		{
			name:   "empty network name",
			modify: func(c *V2Config) { c.Networks[0].Name = "" },
			errMsg: "name cannot be empty",
		},
		{
			name: "duplicate network name",
			modify: func(c *V2Config) {
				c.Networks = append(c.Networks, V2Network{
					Name: "pocket", Type: "cosmos",
					Endpoints: []V2Endpoint{{Protocol: "rpc", Listen: ":9999"}},
				})
			},
			errMsg: "duplicate network name",
		},
		{
			name:   "empty network type",
			modify: func(c *V2Config) { c.Networks[0].Type = "" },
			errMsg: "type cannot be empty",
		},
		{
			name:   "unknown network type",
			modify: func(c *V2Config) { c.Networks[0].Type = "bitcoin" },
			errMsg: "unknown chain type",
		},
		{
			name:   "no endpoints",
			modify: func(c *V2Config) { c.Networks[0].Endpoints = nil },
			errMsg: "at least one endpoint must be configured",
		},
		{
			name:   "empty endpoint protocol",
			modify: func(c *V2Config) { c.Networks[0].Endpoints[0].Protocol = "" },
			errMsg: "protocol cannot be empty",
		},
		{
			name: "duplicate endpoint protocol",
			modify: func(c *V2Config) {
				c.Networks[0].Endpoints = append(c.Networks[0].Endpoints,
					V2Endpoint{Protocol: "rpc", Listen: ":9999"})
			},
			errMsg: "duplicate endpoint protocol",
		},
		{
			name:   "empty endpoint listen",
			modify: func(c *V2Config) { c.Networks[0].Endpoints[0].Listen = "" },
			errMsg: "listen address cannot be empty",
		},
		{
			name:   "invalid endpoint listen",
			modify: func(c *V2Config) { c.Networks[0].Endpoints[0].Listen = "bad" },
			errMsg: "invalid listen format",
		},
		{
			name: "conflicting listen addresses",
			modify: func(c *V2Config) {
				c.Networks = append(c.Networks, V2Network{
					Name: "other", Type: "cosmos",
					Endpoints: []V2Endpoint{{Protocol: "rpc", Listen: ":8081"}},
				})
			},
			errMsg: "conflicts with network",
		},
		{
			name: "invalid advertise URL",
			modify: func(c *V2Config) {
				c.Networks[0].Endpoints[0].Advertise = "not a url %%"
			},
			errMsg: "invalid",
		},
		{
			name: "grpc advertise without port",
			modify: func(c *V2Config) {
				c.Networks[0].Endpoints = []V2Endpoint{
					{Protocol: "grpc", Listen: ":8082", Advertise: "example.com"},
				}
			},
			errMsg: "advertised grpc must include port",
		},

		// Height check interval.
		{
			name: "height check interval too short",
			modify: func(c *V2Config) {
				c.Networks[0].HeightCheck.Interval = 100 * time.Millisecond
			},
			errMsg: "height_check interval too short",
		},

		// EVM mode.
		{
			name: "invalid evm mode",
			modify: func(c *V2Config) {
				c.Networks[0].Type = "evm"
				c.Networks[0].Mode = "invalid"
			},
			errMsg: "invalid evm mode",
		},

		// Custom type.
		{
			name: "custom without height_check",
			modify: func(c *V2Config) {
				c.Networks[0].Type = "custom"
				c.Networks[0].HeightCheck = nil
			},
			errMsg: "custom type requires height_check",
		},
		{
			name: "custom without response_path",
			modify: func(c *V2Config) {
				c.Networks[0].Type = "custom"
				c.Networks[0].HeightCheck = &V2HeightCheck{Method: "GET"}
			},
			errMsg: "custom height_check requires response_path",
		},
		{
			name: "custom without method",
			modify: func(c *V2Config) {
				c.Networks[0].Type = "custom"
				c.Networks[0].HeightCheck = &V2HeightCheck{ResponsePath: ".height"}
			},
			errMsg: "custom height_check requires method",
		},
		{
			name: "custom invalid response_format",
			modify: func(c *V2Config) {
				c.Networks[0].Type = "custom"
				c.Networks[0].HeightCheck = &V2HeightCheck{
					Method: "GET", ResponsePath: ".height", ResponseFormat: "binary",
				}
			},
			errMsg: "invalid response_format",
		},

		// Sync.
		{
			name: "sync zero max_drift",
			modify: func(c *V2Config) {
				c.Networks[0].Sync = &V2SyncCheck{MaxDrift: 0, Oracles: []V2Oracle{{URL: "http://x"}}}
			},
			errMsg: "sync max_drift must be positive",
		},
		{
			name: "sync no oracles",
			modify: func(c *V2Config) {
				c.Networks[0].Sync = &V2SyncCheck{MaxDrift: 5}
			},
			errMsg: "sync requires at least one oracle",
		},
		{
			name: "sync oracle empty URL",
			modify: func(c *V2Config) {
				c.Networks[0].Sync = &V2SyncCheck{MaxDrift: 5, Oracles: []V2Oracle{{URL: ""}}}
			},
			errMsg: "URL cannot be empty",
		},

		// Archival.
		{
			name: "archival negative min_height",
			modify: func(c *V2Config) {
				c.Networks[0].Archival = &V2ArchivalCheck{MinHeight: -1}
			},
			errMsg: "archival min_height cannot be negative",
		},

		// Nodes.
		{
			name:   "no nodes or externals",
			modify: func(c *V2Config) { c.Internals = nil; c.Externals = nil },
			errMsg: "at least one internal node or external ring",
		},
		{
			name:   "node empty name",
			modify: func(c *V2Config) { c.Internals[0].Name = "" },
			errMsg: "name cannot be empty",
		},
		{
			name:   "node empty network",
			modify: func(c *V2Config) { c.Internals[0].Network = "" },
			errMsg: "network cannot be empty",
		},
		{
			name:   "node unknown network",
			modify: func(c *V2Config) { c.Internals[0].Network = "unknown" },
			errMsg: "references unknown network",
		},
		{
			name:   "node no endpoints",
			modify: func(c *V2Config) { c.Internals[0].Endpoints = nil },
			errMsg: "at least one endpoint must be configured",
		},
		{
			name: "node empty endpoint URL",
			modify: func(c *V2Config) {
				c.Internals[0].Endpoints = map[string]string{"rpc": ""}
			},
			errMsg: "URL cannot be empty",
		},
		{
			name: "node grpc without port",
			modify: func(c *V2Config) {
				c.Internals[0].Endpoints = map[string]string{"grpc": "seed-one"}
			},
			errMsg: "grpc endpoint must include port",
		},

		// Users.
		{
			name: "auth without users",
			modify: func(c *V2Config) {
				c.Auth = true
				c.Users = nil
			},
			errMsg: "at least one user must be configured when auth",
		},
		{
			name: "user empty name",
			modify: func(c *V2Config) {
				c.Users = []V2User{{Token: "t", Permissions: V2Permissions{All: true}}}
			},
			errMsg: "name cannot be empty",
		},
		{
			name: "user empty token",
			modify: func(c *V2Config) {
				c.Users = []V2User{{Name: "u", Permissions: V2Permissions{All: true}}}
			},
			errMsg: "token cannot be empty",
		},
		{
			name: "user no permissions",
			modify: func(c *V2Config) {
				c.Users = []V2User{{Name: "u", Token: "t"}}
			},
			errMsg: "permissions must be",
		},
		{
			name: "user unknown network in permissions",
			modify: func(c *V2Config) {
				c.Users = []V2User{{
					Name:  "u",
					Token: "t",
					Permissions: V2Permissions{
						Networks: map[string]map[string]bool{
							"nonexistent": {"rpc": true},
						},
					},
				}}
			},
			errMsg: "unknown network",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validV2Config()
			tt.modify(&cfg)
			err := ValidateV2(&cfg)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.errMsg)
			}
			if !containsStr(err.Error(), tt.errMsg) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.errMsg)
			}
		})
	}
}

func TestV2Permissions_HasAccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		perms    V2Permissions
		network  string
		protocol string
		want     bool
	}{
		{
			name:     "all access",
			perms:    V2Permissions{All: true},
			network:  "pocket",
			protocol: "rpc",
			want:     true,
		},
		{
			name:     "all access any network",
			perms:    V2Permissions{All: true},
			network:  "ethereum",
			protocol: "jsonrpc",
			want:     true,
		},
		{
			name: "specific access granted",
			perms: V2Permissions{
				Networks: map[string]map[string]bool{
					"pocket": {"rpc": true, "rest": true},
				},
			},
			network:  "pocket",
			protocol: "rpc",
			want:     true,
		},
		{
			name: "specific access denied protocol",
			perms: V2Permissions{
				Networks: map[string]map[string]bool{
					"pocket": {"rpc": true},
				},
			},
			network:  "pocket",
			protocol: "grpc",
			want:     false,
		},
		{
			name: "specific access denied network",
			perms: V2Permissions{
				Networks: map[string]map[string]bool{
					"pocket": {"rpc": true},
				},
			},
			network:  "ethereum",
			protocol: "rpc",
			want:     false,
		},
		{
			name:     "nil networks",
			perms:    V2Permissions{},
			network:  "pocket",
			protocol: "rpc",
			want:     false,
		},
		{
			name: "explicit false",
			perms: V2Permissions{
				Networks: map[string]map[string]bool{
					"pocket": {"rpc": false},
				},
			},
			network:  "pocket",
			protocol: "rpc",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.perms.HasAccess(tt.network, tt.protocol)
			if got != tt.want {
				t.Fatalf("HasAccess(%q, %q) = %v, want %v", tt.network, tt.protocol, got, tt.want)
			}
		})
	}
}

func TestV2Config_FindV2Network(t *testing.T) {
	t.Parallel()
	cfg := validV2Config()

	// Found.
	net := cfg.FindV2Network("pocket")
	if net == nil {
		t.Fatal("expected to find network 'pocket'")
	}
	if net.Name != "pocket" {
		t.Fatalf("got %q, want %q", net.Name, "pocket")
	}

	// Not found.
	if cfg.FindV2Network("nonexistent") != nil {
		t.Fatal("expected nil for unknown network")
	}
}

func TestV2Config_FindV2User(t *testing.T) {
	t.Parallel()
	cfg := V2Config{
		Users: []V2User{
			{Name: "alice", Token: "token-a", Permissions: V2Permissions{All: true}},
			{Name: "bob", Token: "token-b", Permissions: V2Permissions{All: true}},
		},
	}

	// Found.
	user := cfg.FindV2User("token-a")
	if user == nil {
		t.Fatal("expected to find user 'alice'")
	}
	if user.Name != "alice" {
		t.Fatalf("got %q, want %q", user.Name, "alice")
	}

	// Found second user.
	user = cfg.FindV2User("token-b")
	if user == nil || user.Name != "bob" {
		t.Fatal("expected to find user 'bob'")
	}

	// Not found.
	if cfg.FindV2User("wrong-token") != nil {
		t.Fatal("expected nil for wrong token")
	}

	// Empty token.
	if cfg.FindV2User("") != nil {
		t.Fatal("expected nil for empty token")
	}
}

func TestV2Network_FindV2Endpoint(t *testing.T) {
	t.Parallel()
	net := V2Network{
		Endpoints: []V2Endpoint{
			{Protocol: "rest", Listen: ":8080"},
			{Protocol: "rpc", Listen: ":8081"},
		},
	}

	ep := net.FindV2Endpoint("rpc")
	if ep == nil {
		t.Fatal("expected to find endpoint 'rpc'")
	}
	if ep.Listen != ":8081" {
		t.Fatalf("got %q, want %q", ep.Listen, ":8081")
	}

	if net.FindV2Endpoint("grpc") != nil {
		t.Fatal("expected nil for missing protocol")
	}
}

func TestV2Network_HeightCheckInterval(t *testing.T) {
	t.Parallel()

	// Default.
	net := V2Network{}
	if got := net.HeightCheckInterval(); got != 30*time.Second {
		t.Fatalf("default interval: got %v, want 30s", got)
	}

	// Custom.
	net = V2Network{HeightCheck: &V2HeightCheck{Interval: 12 * time.Second}}
	if got := net.HeightCheckInterval(); got != 12*time.Second {
		t.Fatalf("custom interval: got %v, want 12s", got)
	}

	// Zero uses default.
	net = V2Network{HeightCheck: &V2HeightCheck{Interval: 0}}
	if got := net.HeightCheckInterval(); got != 30*time.Second {
		t.Fatalf("zero interval: got %v, want 30s", got)
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && searchStr(s, substr)
}

func searchStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
