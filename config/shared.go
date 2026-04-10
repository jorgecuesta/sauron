package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Timeouts configuration for health checks and proxying.
type Timeouts struct {
	HealthCheck time.Duration `mapstructure:"health_check"`
	Proxy       time.Duration `mapstructure:"proxy"`
}

// GRPCKeepalive configures the gRPC client keepalive parameters.
// Controls how often Sauron pings backend gRPC servers to detect dead connections.
// Time must be >= the server's MinPingTime (default 5m in Go gRPC) to avoid
// ENHANCE_YOUR_CALM / too_many_pings rejection.
type GRPCKeepalive struct {
	Time                time.Duration `mapstructure:"time"`                  // How often to ping (default: 600s)
	Timeout             time.Duration `mapstructure:"timeout"`               // Wait for ping ack (default: 20s)
	PermitWithoutStream bool          `mapstructure:"permit_without_stream"` // Ping on idle connections (default: false)
}

// Redis configuration (optional distributed cache).
type Redis struct {
	Enabled bool   `mapstructure:"enabled"`
	URI     string `mapstructure:"uri"`
}

// RateLimit configuration for status API rate limiting.
type RateLimit struct {
	Enabled           bool `mapstructure:"enabled"`             // whether rate limiting is enabled
	RequestsPerSecond int  `mapstructure:"requests_per_second"` // requests allowed per second per IP
	Burst             int  `mapstructure:"burst"`               // burst capacity
	TrustProxy        bool `mapstructure:"trust_proxy"`         // trust X-Forwarded-For and proxy headers
}

// External represents other Sauron deployments for cross-region discovery.
type External struct {
	Name  string   `mapstructure:"name"`
	Token string   `mapstructure:"token"`
	Rings []string `mapstructure:"rings"`
}

// validateListenAddress checks that a listen address has valid format.
func validateListenAddress(addr, fieldName string) error {
	if !strings.HasPrefix(addr, ":") && !strings.HasPrefix(addr, "0.0.0.0:") && !strings.HasPrefix(addr, "127.0.0.1:") {
		return fmt.Errorf("invalid %s format: %s", fieldName, addr)
	}
	return nil
}

// validateURL validates that a URL string is well-formed.
func validateURL(urlStr, typ string) error {
	if !strings.HasPrefix(urlStr, "http://") && !strings.HasPrefix(urlStr, "https://") {
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

// validateExternal validates an external ring configuration.
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
