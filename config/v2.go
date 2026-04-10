package config

import (
	"crypto/subtle"
	"time"
)

// MultiChainConfig represents the complete Sauron multi-chain configuration.
// Supports multi-chain networks with pluggable adapters.
type MultiChainConfig struct {
	Listen        string              `mapstructure:"listen"`
	Auth          bool                `mapstructure:"auth"`
	Timeouts      Timeouts            `mapstructure:"timeouts"`
	GRPCKeepalive GRPCKeepalive       `mapstructure:"grpc_keepalive"`
	Redis         Redis               `mapstructure:"redis"`
	RateLimit     RateLimit           `mapstructure:"rate_limit"`
	Networks      []MultiChainNetwork `mapstructure:"networks"`
	Internals     []MultiChainNode    `mapstructure:"internals"`
	Externals     []External          `mapstructure:"externals"`
	Users         []MultiChainUser    `mapstructure:"users"`

	// ExternalFailoverThreshold: blocks behind before using externals (default: 2).
	ExternalFailoverThreshold int64 `mapstructure:"external_failover_threshold"`
}

// MultiChainNetwork represents a blockchain network with its chain type and endpoints.
type MultiChainNetwork struct {
	Name        string             `mapstructure:"name"`
	Type        string             `mapstructure:"type"` // "cosmos", "evm", "solana", "custom"
	HeightCheck *HeightCheckConfig `mapstructure:"height_check"`
	Endpoints   []NetworkEndpoint  `mapstructure:"endpoints"`
	Mode        string             `mapstructure:"mode"` // EVM: "latest", "safe", "finalized"
	Archival    *ArchivalCheck     `mapstructure:"archival"`
	Sync        *SyncCheck         `mapstructure:"sync"`
}

// HeightCheckConfig configures how to determine a node's block height.
type HeightCheckConfig struct {
	Protocol       string            `mapstructure:"protocol"`        // Which endpoint protocol to use
	Interval       time.Duration     `mapstructure:"interval"`        // Check frequency (default: 30s)
	Method         string            `mapstructure:"method"`          // HTTP method for custom type
	URLPath        string            `mapstructure:"url_path"`        // URL path for custom type
	Headers        map[string]string `mapstructure:"headers"`         // Custom headers
	Body           string            `mapstructure:"body"`            // Request body for custom type
	ResponsePath   string            `mapstructure:"response_path"`   // JSONPath to height value
	ResponseFormat string            `mapstructure:"response_format"` // "integer", "hex", "string"
}

// NetworkEndpoint describes a protocol endpoint for a network.
type NetworkEndpoint struct {
	Protocol           string `mapstructure:"protocol"`               // "rest", "rpc", "grpc", "jsonrpc", "http", "websocket"
	Listen             string `mapstructure:"listen"`                 // Listen address (e.g. ":8080")
	Advertise          string `mapstructure:"advertise"`              // Advertised URL for external rings
	GRPCMaxRecvMsgSize int    `mapstructure:"grpc_max_recv_msg_size"` // gRPC only
	GRPCMaxSendMsgSize int    `mapstructure:"grpc_max_send_msg_size"` // gRPC only
}

// ArchivalCheck configures archival node detection.
type ArchivalCheck struct {
	MinHeight     int64         `mapstructure:"min_height"`
	CheckInterval time.Duration `mapstructure:"check_interval"`
}

// SyncCheck configures sync oracle verification.
type SyncCheck struct {
	MaxDrift      int64         `mapstructure:"max_drift"`
	CheckInterval time.Duration `mapstructure:"check_interval"`
	Oracles       []SyncOracle  `mapstructure:"oracles"`
}

// SyncOracle represents a sync oracle. It can be a simple URL string (same config
// as the network's height_check) or a full override with custom method/path/etc.
type SyncOracle struct {
	URL            string            `mapstructure:"url"`
	Method         string            `mapstructure:"method"`
	URLPath        string            `mapstructure:"url_path"`
	Headers        map[string]string `mapstructure:"headers"`
	Body           string            `mapstructure:"body"`
	ResponsePath   string            `mapstructure:"response_path"`
	ResponseFormat string            `mapstructure:"response_format"`
}

// MultiChainNode represents an internal node with per-protocol endpoints.
type MultiChainNode struct {
	Name         string            `mapstructure:"name"`
	Network      string            `mapstructure:"network"`
	Endpoints    map[string]string `mapstructure:"endpoints"`     // protocol → URL
	GRPCInsecure bool              `mapstructure:"grpc_insecure"` // gRPC uses insecure (no TLS)
}

// MultiChainUser represents an authenticated user with per-network per-protocol permissions.
type MultiChainUser struct {
	Name        string                `mapstructure:"name"`
	Token       string                `mapstructure:"token"`
	Permissions MultiChainPermissions `mapstructure:"permissions"`
}

// MultiChainPermissions can be "all" (full access) or a map of network → protocol → bool.
type MultiChainPermissions struct {
	All      bool                       // If true, user has access to everything.
	Networks map[string]map[string]bool // network → protocol → allowed
}

// HasAccess checks if a user has access to a specific network and protocol.
func (p *MultiChainPermissions) HasAccess(network, protocol string) bool {
	if p.All {
		return true
	}
	if p.Networks == nil {
		return false
	}
	protos, ok := p.Networks[network]
	if !ok {
		return false
	}
	return protos[protocol]
}

// FindNetwork returns the MultiChainNetwork config for the given name, or nil if not found.
func (c *MultiChainConfig) FindNetwork(name string) *MultiChainNetwork {
	for i := range c.Networks {
		if c.Networks[i].Name == name {
			return &c.Networks[i]
		}
	}
	return nil
}

// FindUser finds a user by token using constant-time comparison.
func (c *MultiChainConfig) FindUser(token string) *MultiChainUser {
	for i := range c.Users {
		if subtle.ConstantTimeCompare([]byte(c.Users[i].Token), []byte(token)) == 1 {
			return &c.Users[i]
		}
	}
	return nil
}

// FindEndpoint returns the endpoint config for a given protocol in a network.
func (n *MultiChainNetwork) FindEndpoint(protocol string) *NetworkEndpoint {
	for i := range n.Endpoints {
		if n.Endpoints[i].Protocol == protocol {
			return &n.Endpoints[i]
		}
	}
	return nil
}

// HeightCheckInterval returns the height check interval with a default of 30s.
func (n *MultiChainNetwork) HeightCheckInterval() time.Duration {
	if n.HeightCheck != nil && n.HeightCheck.Interval > 0 {
		return n.HeightCheck.Interval
	}
	return 30 * time.Second
}

// HeightCheckProtocol returns the protocol used for height checks.
// For native adapters (cosmos, evm, solana) without explicit protocol, returns "".
// The adapter factory determines the default in that case.
func (n *MultiChainNetwork) HeightCheckProtocol() string {
	if n.HeightCheck != nil {
		return n.HeightCheck.Protocol
	}
	return ""
}

// GetEnabledTypes returns all unique protocol names across all network endpoints.
// This replaces the V1 approach of global API/RPC/GRPC boolean flags.
func (c *MultiChainConfig) GetEnabledTypes() []string {
	seen := make(map[string]bool)
	var types []string
	for _, net := range c.Networks {
		for _, ep := range net.Endpoints {
			if !seen[ep.Protocol] {
				seen[ep.Protocol] = true
				types = append(types, ep.Protocol)
			}
		}
	}
	return types
}

// GetUserPermissions returns all protocols a user has access to across all networks.
// For "all" permissions, returns GetEnabledTypes(). For specific permissions,
// returns the union of all allowed protocols across networks.
func (c *MultiChainConfig) GetUserPermissions(token string) []string {
	user := c.FindUser(token)
	if user == nil {
		return c.GetEnabledTypes()
	}

	if user.Permissions.All {
		return c.GetEnabledTypes()
	}

	seen := make(map[string]bool)
	var types []string
	for _, protos := range user.Permissions.Networks {
		for proto, allowed := range protos {
			if allowed && !seen[proto] {
				seen[proto] = true
				types = append(types, proto)
			}
		}
	}
	return types
}
