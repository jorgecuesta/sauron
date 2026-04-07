package config

import (
	"strings"
	"testing"
	"time"
)

// minimalValidConfig returns the smallest Config that passes Validate.
func minimalValidConfig() *Config {
	return &Config{
		Listen: ":3000",
		Timeouts: Timeouts{
			HealthCheck: 5 * time.Second,
			Proxy:       30 * time.Second,
		},
		Networks: []Network{
			{Name: "pocket"},
		},
		Internals: []Node{
			{
				Name:    "node-1",
				Network: "pocket",
				API:     "https://node1.example.com",
			},
		},
	}
}

func TestValidate_ValidMinimalConfig(t *testing.T) {
	t.Parallel()
	cfg := minimalValidConfig()
	if err := Validate(cfg); err != nil {
		t.Errorf("expected valid config to pass, got error: %v", err)
	}
}

func TestValidate_MissingListenAddress(t *testing.T) {
	t.Parallel()
	cfg := minimalValidConfig()
	cfg.Listen = ""
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for empty listen address, got nil")
	}
	if !strings.Contains(err.Error(), "listen address cannot be empty") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestValidate_InvalidListenFormat(t *testing.T) {
	t.Parallel()
	cfg := minimalValidConfig()
	cfg.Listen = "nocolon"
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for invalid listen format, got nil")
	}
	if !strings.Contains(err.Error(), "invalid listen address format") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestValidate_ZeroHealthCheckTimeout(t *testing.T) {
	t.Parallel()
	cfg := minimalValidConfig()
	cfg.Timeouts.HealthCheck = 0
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for zero health_check timeout, got nil")
	}
	if !strings.Contains(err.Error(), "health_check timeout cannot be zero") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestValidate_DuplicateNetworkNames(t *testing.T) {
	t.Parallel()
	cfg := minimalValidConfig()
	// Add a second network with the same name
	cfg.Networks = append(cfg.Networks, Network{Name: "pocket"})
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for duplicate network names, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate network name") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestValidate_DuplicateListenAddresses(t *testing.T) {
	t.Parallel()
	cfg := minimalValidConfig()
	// Enable API so api_listen is validated
	cfg.API = true
	cfg.Networks = []Network{
		{Name: "pocket", APIListen: ":8080"},
		{Name: "pocket-beta", APIListen: ":8080"}, // same api_listen
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for duplicate api_listen addresses, got nil")
	}
	if !strings.Contains(err.Error(), "conflicts with") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestValidate_NoInternalsNoExternals(t *testing.T) {
	t.Parallel()
	cfg := minimalValidConfig()
	cfg.Internals = nil
	cfg.Externals = nil
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error when no internals and no externals, got nil")
	}
	if !strings.Contains(err.Error(), "at least one internal node or external ring") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestValidate_NodeWithoutEndpoints(t *testing.T) {
	t.Parallel()
	cfg := minimalValidConfig()
	cfg.Internals = []Node{
		{
			Name:    "node-empty",
			Network: "pocket",
			// API, RPC, GRPC all empty
		},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for node without any endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "at least one endpoint") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestValidate_NodeDuplicateNames(t *testing.T) {
	t.Parallel()
	// The validator doesn't currently check for duplicate node names;
	// test that two nodes with the same name but valid endpoints still pass.
	// If the validator is extended to reject duplicates, this test should be updated.
	cfg := minimalValidConfig()
	cfg.Internals = []Node{
		{Name: "node-1", Network: "pocket", API: "https://n1.example.com"},
		{Name: "node-1", Network: "pocket", API: "https://n1b.example.com"},
	}
	// No duplicate-name check exists in validator today — both nodes are valid individually.
	if err := Validate(cfg); err != nil {
		t.Logf("Validate returned error (may be expected if duplicate check added): %v", err)
	}
}

func TestValidate_UserWithoutPermissions(t *testing.T) {
	t.Parallel()
	cfg := minimalValidConfig()
	cfg.Auth = true
	cfg.Users = []User{
		{
			Name:  "alice",
			Token: "tok123",
			API:   false,
			RPC:   false,
			GRPC:  false,
		},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for user with no permissions, got nil")
	}
	if !strings.Contains(err.Error(), "at least one permission") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestValidate_UserWithoutToken(t *testing.T) {
	t.Parallel()
	cfg := minimalValidConfig()
	cfg.Auth = true
	cfg.Users = []User{
		{
			Name:  "bob",
			Token: "", // empty token
			API:   true,
		},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for user with empty token, got nil")
	}
	if !strings.Contains(err.Error(), "token cannot be empty") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestValidate_ValidFullConfig(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		API:                       true,
		RPC:                       true,
		GRPC:                      true,
		Auth:                      true,
		Listen:                    "0.0.0.0:3000",
		ExternalFailoverThreshold: 2,
		Timeouts: Timeouts{
			HealthCheck: 10 * time.Second,
			Proxy:       60 * time.Second,
		},
		Redis: Redis{
			Enabled: true,
			URI:     "redis://localhost:6379",
		},
		Networks: []Network{
			{
				Name:       "pocket",
				APIListen:  ":8080",
				RPCListen:  ":8081",
				GRPCListen: ":8082",
				API:        "https://api.pocket.example.com",
				RPC:        "https://rpc.pocket.example.com",
				GRPC:       "grpc.pocket.example.com:9090",
			},
		},
		Internals: []Node{
			{
				Name:    "node-1",
				Network: "pocket",
				API:     "https://node1.example.com",
				RPC:     "https://node1.example.com:26657",
				GRPC:    "node1.example.com:9090",
			},
		},
		Externals: []External{
			{
				Name:  "ext-ring",
				Token: "exttoken",
				Rings: []string{"https://ring1.example.com"},
			},
		},
		Users: []User{
			{
				Name:  "admin",
				Token: "supersecrettoken",
				API:   true,
				RPC:   true,
				GRPC:  true,
			},
		},
	}
	if err := Validate(cfg); err != nil {
		t.Errorf("expected full valid config to pass, got: %v", err)
	}
}
