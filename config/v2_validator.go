package config

import (
	"fmt"
	"strings"
	"time"
)

// knownChainTypes lists the built-in adapter types.
var knownChainTypes = map[string]bool{
	"cosmos": true,
	"evm":    true,
	"solana": true,
	"custom": true,
}

// ValidateV2 checks if a V2Config is valid.
func ValidateV2(cfg *V2Config) error {
	if cfg.Listen == "" {
		return fmt.Errorf("listen address cannot be empty")
	}
	if err := validateListenAddress(cfg.Listen, "listen"); err != nil {
		return err
	}

	if err := validateV2Timeouts(&cfg.Timeouts); err != nil {
		return err
	}

	if cfg.Redis.Enabled {
		if cfg.Redis.URI == "" {
			return fmt.Errorf("redis URI cannot be empty when redis is enabled")
		}
		if !strings.HasPrefix(cfg.Redis.URI, "redis://") && !strings.HasPrefix(cfg.Redis.URI, "rediss://") {
			return fmt.Errorf("invalid redis URI format: %s", cfg.Redis.URI)
		}
	}

	if len(cfg.Networks) == 0 {
		return fmt.Errorf("at least one network must be configured")
	}

	networkNames := make(map[string]bool)
	listenAddrs := make(map[string]string) // address → network name

	for i := range cfg.Networks {
		if err := validateV2Network(&cfg.Networks[i], i, networkNames, listenAddrs); err != nil {
			return err
		}
	}

	if len(cfg.Internals) == 0 && len(cfg.Externals) == 0 {
		return fmt.Errorf("at least one internal node or external ring must be configured")
	}

	// Build set of valid network names for node validation.
	for i := range cfg.Internals {
		if err := validateV2Node(&cfg.Internals[i], i, networkNames); err != nil {
			return err
		}
	}

	for i := range cfg.Externals {
		if err := validateExternal(&cfg.Externals[i], i); err != nil {
			return err
		}
	}

	if cfg.Auth && len(cfg.Users) == 0 {
		return fmt.Errorf("at least one user must be configured when auth is enabled")
	}
	for i := range cfg.Users {
		if err := validateV2User(&cfg.Users[i], i, networkNames); err != nil {
			return err
		}
	}

	return nil
}

func validateV2Timeouts(t *Timeouts) error {
	if t.HealthCheck == 0 {
		return fmt.Errorf("health_check timeout cannot be zero")
	}
	if t.HealthCheck < time.Second {
		return fmt.Errorf("health_check timeout too short: %s (minimum 1s)", t.HealthCheck)
	}
	if t.Proxy == 0 {
		return fmt.Errorf("proxy timeout cannot be zero")
	}
	if t.Proxy < time.Second {
		return fmt.Errorf("proxy timeout too short: %s (minimum 1s)", t.Proxy)
	}
	return nil
}

func validateV2Network(net *V2Network, index int, names map[string]bool, addrs map[string]string) error {
	if net.Name == "" {
		return fmt.Errorf("network %d: name cannot be empty", index)
	}
	if names[net.Name] {
		return fmt.Errorf("network %d: duplicate network name %q", index, net.Name)
	}
	names[net.Name] = true

	if net.Type == "" {
		return fmt.Errorf("network %d (%s): type cannot be empty", index, net.Name)
	}
	if !knownChainTypes[net.Type] {
		return fmt.Errorf("network %d (%s): unknown chain type %q", index, net.Name, net.Type)
	}

	// Custom type requires height_check config.
	if net.Type == "custom" {
		if err := validateCustomHeightCheck(net, index); err != nil {
			return err
		}
	}

	// EVM mode validation.
	if net.Type == "evm" && net.Mode != "" {
		validModes := map[string]bool{"latest": true, "safe": true, "finalized": true}
		if !validModes[net.Mode] {
			return fmt.Errorf("network %d (%s): invalid evm mode %q (must be latest, safe, or finalized)", index, net.Name, net.Mode)
		}
	}

	// HeightCheck interval validation.
	if net.HeightCheck != nil && net.HeightCheck.Interval > 0 && net.HeightCheck.Interval < time.Second {
		return fmt.Errorf("network %d (%s): height_check interval too short: %s (minimum 1s)", index, net.Name, net.HeightCheck.Interval)
	}

	// Endpoints validation.
	if len(net.Endpoints) == 0 {
		return fmt.Errorf("network %d (%s): at least one endpoint must be configured", index, net.Name)
	}

	endpointProtos := make(map[string]bool)
	for j := range net.Endpoints {
		ep := &net.Endpoints[j]
		if ep.Protocol == "" {
			return fmt.Errorf("network %d (%s), endpoint %d: protocol cannot be empty", index, net.Name, j)
		}
		if endpointProtos[ep.Protocol] {
			return fmt.Errorf("network %d (%s): duplicate endpoint protocol %q", index, net.Name, ep.Protocol)
		}
		endpointProtos[ep.Protocol] = true

		if ep.Listen == "" {
			return fmt.Errorf("network %d (%s), endpoint %q: listen address cannot be empty", index, net.Name, ep.Protocol)
		}
		if err := validateListenAddress(ep.Listen, "listen"); err != nil {
			return fmt.Errorf("network %d (%s), endpoint %q: %w", index, net.Name, ep.Protocol, err)
		}

		// Check listen address uniqueness across all networks.
		if existingNet, exists := addrs[ep.Listen]; exists {
			return fmt.Errorf("network %d (%s), endpoint %q: listen %q conflicts with network %q", index, net.Name, ep.Protocol, ep.Listen, existingNet)
		}
		addrs[ep.Listen] = net.Name

		// Validate advertised URL if set.
		if ep.Advertise != "" {
			if ep.Protocol == "grpc" {
				if !strings.Contains(ep.Advertise, ":") {
					return fmt.Errorf("network %d (%s), endpoint %q: advertised grpc must include port", index, net.Name, ep.Protocol)
				}
			} else {
				if err := validateURL(ep.Advertise, "advertise"); err != nil {
					return fmt.Errorf("network %d (%s), endpoint %q: %w", index, net.Name, ep.Protocol, err)
				}
			}
		}
	}

	// Sync oracle validation.
	if net.Sync != nil {
		if net.Sync.MaxDrift <= 0 {
			return fmt.Errorf("network %d (%s): sync max_drift must be positive", index, net.Name)
		}
		if len(net.Sync.Oracles) == 0 {
			return fmt.Errorf("network %d (%s): sync requires at least one oracle", index, net.Name)
		}
		for j, o := range net.Sync.Oracles {
			if o.URL == "" {
				return fmt.Errorf("network %d (%s), oracle %d: URL cannot be empty", index, net.Name, j)
			}
		}
	}

	// Archival validation.
	if net.Archival != nil {
		if net.Archival.MinHeight < 0 {
			return fmt.Errorf("network %d (%s): archival min_height cannot be negative", index, net.Name)
		}
	}

	return nil
}

func validateCustomHeightCheck(net *V2Network, index int) error {
	if net.HeightCheck == nil {
		return fmt.Errorf("network %d (%s): custom type requires height_check configuration", index, net.Name)
	}
	hc := net.HeightCheck
	if hc.ResponsePath == "" {
		return fmt.Errorf("network %d (%s): custom height_check requires response_path", index, net.Name)
	}
	if hc.Method == "" {
		return fmt.Errorf("network %d (%s): custom height_check requires method", index, net.Name)
	}
	validFormats := map[string]bool{"integer": true, "hex": true, "string": true, "": true}
	if !validFormats[hc.ResponseFormat] {
		return fmt.Errorf("network %d (%s): invalid response_format %q", index, net.Name, hc.ResponseFormat)
	}
	return nil
}

func validateV2Node(node *V2Node, index int, networkNames map[string]bool) error {
	if node.Name == "" {
		return fmt.Errorf("internal node %d: name cannot be empty", index)
	}
	if node.Network == "" {
		return fmt.Errorf("internal node %d (%s): network cannot be empty", index, node.Name)
	}
	if !networkNames[node.Network] {
		return fmt.Errorf("internal node %d (%s): references unknown network %q", index, node.Name, node.Network)
	}
	if len(node.Endpoints) == 0 {
		return fmt.Errorf("internal node %d (%s): at least one endpoint must be configured", index, node.Name)
	}

	for proto, url := range node.Endpoints {
		if url == "" {
			return fmt.Errorf("internal node %d (%s): endpoint %q URL cannot be empty", index, node.Name, proto)
		}
		if proto == "grpc" {
			if !strings.Contains(url, ":") {
				return fmt.Errorf("internal node %d (%s): grpc endpoint must include port", index, node.Name)
			}
		} else {
			if err := validateURL(url, proto); err != nil {
				return fmt.Errorf("internal node %d (%s): %w", index, node.Name, err)
			}
		}
	}

	return nil
}

func validateV2User(user *V2User, index int, networkNames map[string]bool) error {
	if user.Name == "" {
		return fmt.Errorf("user %d: name cannot be empty", index)
	}
	if user.Token == "" {
		return fmt.Errorf("user %d (%s): token cannot be empty", index, user.Name)
	}

	// If not "all", must have at least one permission.
	if !user.Permissions.All && len(user.Permissions.Networks) == 0 {
		return fmt.Errorf("user %d (%s): permissions must be 'all' or specify network access", index, user.Name)
	}

	// Validate referenced networks exist.
	for netName := range user.Permissions.Networks {
		if !networkNames[netName] {
			return fmt.Errorf("user %d (%s): permissions reference unknown network %q", index, user.Name, netName)
		}
	}

	return nil
}
