package config

import (
	"crypto/subtle"
	"time"
)

// Config represents the complete Sauron configuration
// The Dark Tower's ancient scrolls
type Config struct {
	API       bool       `mapstructure:"api"`
	RPC       bool       `mapstructure:"rpc"`
	GRPC      bool       `mapstructure:"grpc"`
	Auth      bool       `mapstructure:"auth"`
	Listen    string     `mapstructure:"listen"`
	Timeouts  Timeouts   `mapstructure:"timeouts"`
	Redis     Redis      `mapstructure:"redis"`
	RateLimit RateLimit  `mapstructure:"rate_limit"`
	Networks  []Network  `mapstructure:"networks"`
	Internals []Node     `mapstructure:"internals"`
	Externals []External `mapstructure:"externals"`
	Users     []User     `mapstructure:"users"`
}

// Timeouts configuration for health checks and proxying
// The Eye's patience
type Timeouts struct {
	HealthCheck time.Duration `mapstructure:"health_check"`
	Proxy       time.Duration `mapstructure:"proxy"`
}

// Redis configuration (optional distributed cache)
// The vaults beneath the tower
type Redis struct {
	Enabled bool   `mapstructure:"enabled"`
	URI     string `mapstructure:"uri"`
}

// RateLimit configuration for status API rate limiting
// The gates' watchful guard
type RateLimit struct {
	Enabled           bool `mapstructure:"enabled"`             // whether rate limiting is enabled
	RequestsPerSecond int  `mapstructure:"requests_per_second"` // requests allowed per second per IP
	Burst             int  `mapstructure:"burst"`               // burst capacity
	TrustProxy        bool `mapstructure:"trust_proxy"`         // trust X-Forwarded-For and proxy headers
}

// Network configuration for per-network proxy listeners
// Each gate leads to a different realm
type Network struct {
	Name               string `mapstructure:"name"`
	API                string `mapstructure:"api"`
	APIListen          string `mapstructure:"api_listen"`
	RPC                string `mapstructure:"rpc"`
	RPCListen          string `mapstructure:"rpc_listen"`
	GRPC               string `mapstructure:"grpc"`
	GRPCListen         string `mapstructure:"grpc_listen"`
	GRPCInsecure       bool   `mapstructure:"grpc_insecure"`
	GRPCMaxRecvMsgSize int    `mapstructure:"grpc_max_recv_msg_size"` // Max message size in bytes (0 = unlimited, default 100MB)
	GRPCMaxSendMsgSize int    `mapstructure:"grpc_max_send_msg_size"` // Max message size in bytes (0 = unlimited, default 100MB)
}

// Node represents an internal node to monitor
// The kingdoms under the Eye's gaze
type Node struct {
	Name         string `mapstructure:"name"`
	API          string `mapstructure:"api"`
	RPC          string `mapstructure:"rpc"`
	GRPC         string `mapstructure:"grpc"`
	GRPCInsecure bool   `mapstructure:"grpc_insecure"` // Whether this node's gRPC endpoint uses insecure (no TLS)
	Network      string `mapstructure:"network"`
}

// External represents other Sauron deployments
// The Palantíri - seeing-stones to distant towers
type External struct {
	Name  string   `mapstructure:"name"`
	Token string   `mapstructure:"token"`
	Rings []string `mapstructure:"rings"`
}

// User represents an authenticated user for the status API
// Those who may peer into the Palantír
type User struct {
	Name  string `mapstructure:"name"`
	Token string `mapstructure:"token"`
	API   bool   `mapstructure:"api"`
	RPC   bool   `mapstructure:"rpc"`
	GRPC  bool   `mapstructure:"grpc"`
}

// GetEnabledTypes returns which endpoint types are globally enabled
func (c *Config) GetEnabledTypes() []string {
	var types []string
	if c.API {
		types = append(types, "api")
	}
	if c.RPC {
		types = append(types, "rpc")
	}
	if c.GRPC {
		types = append(types, "grpc")
	}
	return types
}

// GetUserPermissions returns the enabled types for a specific user
// If not overridden, returns global enabled types
func (c *Config) GetUserPermissions(token string) []string {
	for _, user := range c.Users {
		if user.Token == token {
			var types []string
			if user.API {
				types = append(types, "api")
			}
			if user.RPC {
				types = append(types, "rpc")
			}
			if user.GRPC {
				types = append(types, "grpc")
			}
			return types
		}
	}
	return c.GetEnabledTypes()
}

// FindUser finds a user by token using constant-time comparison to prevent timing attacks
func (c *Config) FindUser(token string) *User {
	for _, user := range c.Users {
		if subtle.ConstantTimeCompare([]byte(user.Token), []byte(token)) == 1 {
			return &user
		}
	}
	return nil
}
