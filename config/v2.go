package config

import (
	"crypto/subtle"
	"time"
)

// V2Config represents the complete Sauron V2 configuration.
// Supports multi-chain networks with pluggable adapters.
type V2Config struct {
	Listen        string        `mapstructure:"listen"`
	Auth          bool          `mapstructure:"auth"`
	Timeouts      Timeouts      `mapstructure:"timeouts"`
	GRPCKeepalive GRPCKeepalive `mapstructure:"grpc_keepalive"`
	Redis         Redis         `mapstructure:"redis"`
	RateLimit     RateLimit     `mapstructure:"rate_limit"`
	Networks      []V2Network   `mapstructure:"networks"`
	Internals     []V2Node      `mapstructure:"internals"`
	Externals     []External    `mapstructure:"externals"`
	Users         []V2User      `mapstructure:"users"`

	// ExternalFailoverThreshold: blocks behind before using externals (default: 2).
	ExternalFailoverThreshold int64 `mapstructure:"external_failover_threshold"`
}

// V2Network represents a blockchain network with its chain type and endpoints.
type V2Network struct {
	Name        string           `mapstructure:"name"`
	Type        string           `mapstructure:"type"` // "cosmos", "evm", "solana", "custom"
	HeightCheck *V2HeightCheck   `mapstructure:"height_check"`
	Endpoints   []V2Endpoint     `mapstructure:"endpoints"`
	Mode        string           `mapstructure:"mode"` // EVM: "latest", "safe", "finalized"
	Archival    *V2ArchivalCheck `mapstructure:"archival"`
	Sync        *V2SyncCheck     `mapstructure:"sync"`
}

// V2HeightCheck configures how to determine a node's block height.
type V2HeightCheck struct {
	Protocol       string            `mapstructure:"protocol"`        // Which endpoint protocol to use
	Interval       time.Duration     `mapstructure:"interval"`        // Check frequency (default: 30s)
	Method         string            `mapstructure:"method"`          // HTTP method for custom type
	URLPath        string            `mapstructure:"url_path"`        // URL path for custom type
	Headers        map[string]string `mapstructure:"headers"`         // Custom headers
	Body           string            `mapstructure:"body"`            // Request body for custom type
	ResponsePath   string            `mapstructure:"response_path"`   // JSONPath to height value
	ResponseFormat string            `mapstructure:"response_format"` // "integer", "hex", "string"
}

// V2Endpoint describes a protocol endpoint for a network.
type V2Endpoint struct {
	Protocol           string `mapstructure:"protocol"`               // "rest", "rpc", "grpc", "jsonrpc", "http", "websocket"
	Listen             string `mapstructure:"listen"`                 // Listen address (e.g. ":8080")
	Advertise          string `mapstructure:"advertise"`              // Advertised URL for external rings
	GRPCMaxRecvMsgSize int    `mapstructure:"grpc_max_recv_msg_size"` // gRPC only
	GRPCMaxSendMsgSize int    `mapstructure:"grpc_max_send_msg_size"` // gRPC only
}

// V2ArchivalCheck configures archival node detection.
type V2ArchivalCheck struct {
	MinHeight     int64         `mapstructure:"min_height"`
	CheckInterval time.Duration `mapstructure:"check_interval"`
}

// V2SyncCheck configures sync oracle verification.
type V2SyncCheck struct {
	MaxDrift      int64         `mapstructure:"max_drift"`
	CheckInterval time.Duration `mapstructure:"check_interval"`
	Oracles       []V2Oracle    `mapstructure:"oracles"`
}

// V2Oracle represents a sync oracle. It can be a simple URL string (same config
// as the network's height_check) or a full override with custom method/path/etc.
type V2Oracle struct {
	URL            string            `mapstructure:"url"`
	Method         string            `mapstructure:"method"`
	URLPath        string            `mapstructure:"url_path"`
	Headers        map[string]string `mapstructure:"headers"`
	Body           string            `mapstructure:"body"`
	ResponsePath   string            `mapstructure:"response_path"`
	ResponseFormat string            `mapstructure:"response_format"`
}

// V2Node represents an internal node with per-protocol endpoints.
type V2Node struct {
	Name         string            `mapstructure:"name"`
	Network      string            `mapstructure:"network"`
	Endpoints    map[string]string `mapstructure:"endpoints"`     // protocol → URL
	GRPCInsecure bool              `mapstructure:"grpc_insecure"` // gRPC uses insecure (no TLS)
}

// V2User represents an authenticated user with per-network per-protocol permissions.
type V2User struct {
	Name        string        `mapstructure:"name"`
	Token       string        `mapstructure:"token"`
	Permissions V2Permissions `mapstructure:"permissions"`
}

// V2Permissions can be "all" (full access) or a map of network → protocol → bool.
type V2Permissions struct {
	All      bool                       // If true, user has access to everything.
	Networks map[string]map[string]bool // network → protocol → allowed
}

// HasAccess checks if a user has access to a specific network and protocol.
func (p *V2Permissions) HasAccess(network, protocol string) bool {
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

// FindV2Network returns the V2Network config for the given name, or nil if not found.
func (c *V2Config) FindV2Network(name string) *V2Network {
	for i := range c.Networks {
		if c.Networks[i].Name == name {
			return &c.Networks[i]
		}
	}
	return nil
}

// FindV2User finds a user by token using constant-time comparison.
func (c *V2Config) FindV2User(token string) *V2User {
	for i := range c.Users {
		if subtle.ConstantTimeCompare([]byte(c.Users[i].Token), []byte(token)) == 1 {
			return &c.Users[i]
		}
	}
	return nil
}

// FindV2Endpoint returns the endpoint config for a given protocol in a network.
func (n *V2Network) FindV2Endpoint(protocol string) *V2Endpoint {
	for i := range n.Endpoints {
		if n.Endpoints[i].Protocol == protocol {
			return &n.Endpoints[i]
		}
	}
	return nil
}

// HeightCheckInterval returns the height check interval with a default of 30s.
func (n *V2Network) HeightCheckInterval() time.Duration {
	if n.HeightCheck != nil && n.HeightCheck.Interval > 0 {
		return n.HeightCheck.Interval
	}
	return 30 * time.Second
}
