package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Validate checks if the configuration is valid
// The Dark Lord's judgment upon the scrolls
func Validate(cfg *Config) error {
	// Validate listen address
	if cfg.Listen == "" {
		return fmt.Errorf("listen address cannot be empty")
	}
	if !strings.HasPrefix(cfg.Listen, ":") && !strings.HasPrefix(cfg.Listen, "0.0.0.0:") && !strings.HasPrefix(cfg.Listen, "127.0.0.1:") {
		return fmt.Errorf("invalid listen address format: %s", cfg.Listen)
	}

	// Validate timeouts
	if cfg.Timeouts.HealthCheck == 0 {
		return fmt.Errorf("health_check timeout cannot be zero")
	}
	if cfg.Timeouts.HealthCheck < time.Second {
		return fmt.Errorf("health_check timeout too short: %s (minimum 1s)", cfg.Timeouts.HealthCheck)
	}
	if cfg.Timeouts.Proxy == 0 {
		return fmt.Errorf("proxy timeout cannot be zero")
	}
	if cfg.Timeouts.Proxy < time.Second {
		return fmt.Errorf("proxy timeout too short: %s (minimum 1s)", cfg.Timeouts.Proxy)
	}

	// Validate Redis if enabled
	if cfg.Redis.Enabled {
		if cfg.Redis.URI == "" {
			return fmt.Errorf("redis URI cannot be empty when redis is enabled")
		}
		if !strings.HasPrefix(cfg.Redis.URI, "redis://") && !strings.HasPrefix(cfg.Redis.URI, "rediss://") {
			return fmt.Errorf("invalid redis URI format: %s", cfg.Redis.URI)
		}
	}

	// Validate networks configuration
	if len(cfg.Networks) == 0 {
		return fmt.Errorf("at least one network must be configured")
	}

	// Track network names and listen addresses to ensure uniqueness
	networkNames := make(map[string]bool)
	listenAddrs := make(map[string]string) // address -> network name

	for i, network := range cfg.Networks {
		if err := validateNetwork(&network, cfg, i, networkNames, listenAddrs); err != nil {
			return err
		}
	}

	// Validate internal nodes
	if len(cfg.Internals) == 0 {
		return fmt.Errorf("at least one internal node must be configured")
	}
	for i, node := range cfg.Internals {
		if err := validateNode(&node, i); err != nil {
			return err
		}
	}

	// Validate external rings
	for i, ext := range cfg.Externals {
		if err := validateExternal(&ext, i); err != nil {
			return err
		}
	}

	// Validate users if auth is enabled
	if cfg.Auth && len(cfg.Users) == 0 {
		return fmt.Errorf("at least one user must be configured when auth is enabled")
	}
	for i, user := range cfg.Users {
		if err := validateUser(&user, i); err != nil {
			return err
		}
	}

	return nil
}

func validateNode(node *Node, index int) error {
	if node.Name == "" {
		return fmt.Errorf("internal node %d: name cannot be empty", index)
	}
	if node.Network == "" {
		return fmt.Errorf("internal node %d (%s): network cannot be empty", index, node.Name)
	}

	// At least one endpoint type must be configured
	if node.API == "" && node.RPC == "" && node.GRPC == "" {
		return fmt.Errorf("internal node %d (%s): at least one endpoint (api/rpc/grpc) must be configured", index, node.Name)
	}

	// Validate URLs
	if node.API != "" {
		if err := validateURL(node.API, "api"); err != nil {
			return fmt.Errorf("internal node %d (%s): %w", index, node.Name, err)
		}
	}
	if node.RPC != "" {
		if err := validateURL(node.RPC, "rpc"); err != nil {
			return fmt.Errorf("internal node %d (%s): %w", index, node.Name, err)
		}
	}
	if node.GRPC != "" {
		// GRPC can be host:port or https://host:port
		if !strings.Contains(node.GRPC, ":") {
			return fmt.Errorf("internal node %d (%s): grpc endpoint must include port", index, node.Name)
		}
	}

	return nil
}

func validateExternal(ext *External, index int) error {
	if ext.Name == "" {
		return fmt.Errorf("external %d: name cannot be empty", index)
	}
	if len(ext.Rings) == 0 {
		return fmt.Errorf("external %d (%s): at least one ring URL must be configured", index, ext.Name)
	}

	for i, ring := range ext.Rings {
		if ring == "" {
			return fmt.Errorf("external %d (%s): ring %d URL cannot be empty", index, ext.Name, i)
		}
		if err := validateURL(ring, "ring"); err != nil {
			return fmt.Errorf("external %d (%s), ring %d: %w", index, ext.Name, i, err)
		}
	}

	return nil
}

func validateUser(user *User, index int) error {
	if user.Name == "" {
		return fmt.Errorf("user %d: name cannot be empty", index)
	}
	if user.Token == "" {
		return fmt.Errorf("user %d (%s): token cannot be empty", index, user.Name)
	}

	// At least one permission must be granted
	if !user.API && !user.RPC && !user.GRPC {
		return fmt.Errorf("user %d (%s): at least one permission (api/rpc/grpc) must be granted", index, user.Name)
	}

	return nil
}

func validateNetwork(network *Network, cfg *Config, index int, networkNames map[string]bool, listenAddrs map[string]string) error {
	if network.Name == "" {
		return fmt.Errorf("network %d: name cannot be empty", index)
	}

	// Check for duplicate network names
	if networkNames[network.Name] {
		return fmt.Errorf("network %d: duplicate network name '%s'", index, network.Name)
	}
	networkNames[network.Name] = true

	// Validate API configuration
	if cfg.API {
		if network.APIListen == "" {
			return fmt.Errorf("network %d (%s): api_listen cannot be empty when API is globally enabled", index, network.Name)
		}
		if err := validateListenAddress(network.APIListen, "api_listen"); err != nil {
			return fmt.Errorf("network %d (%s): %w", index, network.Name, err)
		}
		// Check for duplicate listen addresses
		if existingNet, exists := listenAddrs[network.APIListen]; exists {
			return fmt.Errorf("network %d (%s): api_listen '%s' conflicts with network '%s'", index, network.Name, network.APIListen, existingNet)
		}
		listenAddrs[network.APIListen] = network.Name

		// Validate advertised API URL
		if network.API != "" {
			if err := validateURL(network.API, "api"); err != nil {
				return fmt.Errorf("network %d (%s): advertised %w", index, network.Name, err)
			}
		}
	}

	// Validate RPC configuration
	if cfg.RPC {
		if network.RPCListen == "" {
			return fmt.Errorf("network %d (%s): rpc_listen cannot be empty when RPC is globally enabled", index, network.Name)
		}
		if err := validateListenAddress(network.RPCListen, "rpc_listen"); err != nil {
			return fmt.Errorf("network %d (%s): %w", index, network.Name, err)
		}
		// Check for duplicate listen addresses
		if existingNet, exists := listenAddrs[network.RPCListen]; exists {
			return fmt.Errorf("network %d (%s): rpc_listen '%s' conflicts with network '%s'", index, network.Name, network.RPCListen, existingNet)
		}
		listenAddrs[network.RPCListen] = network.Name

		// Validate advertised RPC URL
		if network.RPC != "" {
			if err := validateURL(network.RPC, "rpc"); err != nil {
				return fmt.Errorf("network %d (%s): advertised %w", index, network.Name, err)
			}
		}
	}

	// Validate GRPC configuration
	if cfg.GRPC {
		if network.GRPCListen == "" {
			return fmt.Errorf("network %d (%s): grpc_listen cannot be empty when GRPC is globally enabled", index, network.Name)
		}
		if err := validateListenAddress(network.GRPCListen, "grpc_listen"); err != nil {
			return fmt.Errorf("network %d (%s): %w", index, network.Name, err)
		}
		// Check for duplicate listen addresses
		if existingNet, exists := listenAddrs[network.GRPCListen]; exists {
			return fmt.Errorf("network %d (%s): grpc_listen '%s' conflicts with network '%s'", index, network.Name, network.GRPCListen, existingNet)
		}
		listenAddrs[network.GRPCListen] = network.Name

		// Validate advertised GRPC endpoint - must include port
		if network.GRPC != "" && !strings.Contains(network.GRPC, ":") {
			return fmt.Errorf("network %d (%s): advertised grpc endpoint must include port", index, network.Name)
		}
	}

	return nil
}

func validateListenAddress(addr, fieldName string) error {
	if !strings.HasPrefix(addr, ":") && !strings.HasPrefix(addr, "0.0.0.0:") && !strings.HasPrefix(addr, "127.0.0.1:") {
		return fmt.Errorf("invalid %s format: %s", fieldName, addr)
	}
	return nil
}

func validateURL(urlStr, typ string) error {
	// Handle cases where URL might not have a scheme
	if !strings.HasPrefix(urlStr, "http://") && !strings.HasPrefix(urlStr, "https://") {
		// Assume https for validation
		urlStr = "https://" + urlStr
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		return fmt.Errorf("invalid %s URL: %w", typ, err)
	}

	if u.Host == "" {
		return fmt.Errorf("invalid %s URL: missing host", typ)
	}

	return nil
}
