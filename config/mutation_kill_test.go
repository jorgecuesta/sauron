package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Tests in this file pin behaviours that mutation testing (issue #19) showed
// were unobserved: exact validation limits, the order in which ValidateMultiChain
// runs its sections, the loaded default interval, and LogReloadResult levels.

func validExternal(name string) External {
	return External{Name: name, Rings: []string{"https://partner-sauron.example.com:3000"}}
}

// ValidateMultiChain keeps validating after a valid external: a bad user or a
// missing user with auth on, or a bad second external, must still be reported.
// Pins the `err != nil` check after validateExternal (v2_validator.go:68).
func TestValidateMultiChain_ExternalsDoNotEndValidationEarly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		modify  func(*MultiChainConfig)
		wantErr string
	}{
		{
			name: "valid external then user without a name",
			modify: func(c *MultiChainConfig) {
				c.Externals = []External{validExternal("partner")}
				c.Users = []MultiChainUser{{Token: "t", Permissions: MultiChainPermissions{All: true}}}
			},
			wantErr: "user 0: name cannot be empty",
		},
		{
			name: "valid external then user with unknown network",
			modify: func(c *MultiChainConfig) {
				c.Externals = []External{validExternal("partner")}
				c.Users = []MultiChainUser{{Name: "u", Token: "t", Permissions: MultiChainPermissions{Networks: map[string]map[string]bool{"nope": {"rpc": true}}}}}
			},
			wantErr: `permissions reference unknown network "nope"`,
		},
		{
			name: "valid external then auth enabled without users",
			modify: func(c *MultiChainConfig) {
				c.Externals = []External{validExternal("partner")}
				c.Auth = true
			},
			wantErr: "at least one user must be configured when auth is enabled",
		},
		{
			name: "valid external then invalid second external",
			modify: func(c *MultiChainConfig) {
				c.Externals = []External{validExternal("partner"), {Name: "broken"}}
			},
			wantErr: "external 1 (broken): at least one ring URL must be configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validMultiChainConfig()
			tt.modify(&cfg)
			err := ValidateMultiChain(&cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// Validation limits are inclusive at their boundary: exactly 1s is accepted
// for health_check, proxy, archival.check_interval and sync.check_interval,
// exactly 0 for archival.min_height, and 100 / 599 for circuit-breaker HTTP
// codes; one step outside is rejected. Pins v2_validator.go:89, 95, 188, 191,
// 212, 237.
func TestValidateMultiChain_BoundaryValues(t *testing.T) {
	t.Parallel()

	oracle := []SyncOracle{{URL: "https://oracle.example.com"}}
	tests := []struct {
		name    string
		modify  func(*MultiChainConfig)
		wantErr string
	}{
		{name: "health_check exactly 1s", modify: func(c *MultiChainConfig) { c.Timeouts.HealthCheck = time.Second }},
		{name: "health_check 999ms", modify: func(c *MultiChainConfig) { c.Timeouts.HealthCheck = 999 * time.Millisecond }, wantErr: "health_check timeout too short"},
		{name: "proxy exactly 1s", modify: func(c *MultiChainConfig) { c.Timeouts.Proxy = time.Second }},
		{name: "proxy 999ms", modify: func(c *MultiChainConfig) { c.Timeouts.Proxy = 999 * time.Millisecond }, wantErr: "proxy timeout too short"},
		{name: "archival check_interval exactly 1s", modify: func(c *MultiChainConfig) {
			c.Networks[0].Archival = &ArchivalCheck{MinHeight: 1, CheckInterval: time.Second}
		}},
		{name: "archival check_interval 999ms", modify: func(c *MultiChainConfig) {
			c.Networks[0].Archival = &ArchivalCheck{MinHeight: 1, CheckInterval: 999 * time.Millisecond}
		}, wantErr: "archival check_interval too short"},
		{name: "sync check_interval exactly 1s", modify: func(c *MultiChainConfig) {
			c.Networks[0].Sync = &SyncCheck{MaxDrift: 5, CheckInterval: time.Second, Oracles: oracle}
		}},
		{name: "sync check_interval 999ms", modify: func(c *MultiChainConfig) {
			c.Networks[0].Sync = &SyncCheck{MaxDrift: 5, CheckInterval: 999 * time.Millisecond, Oracles: oracle}
		}, wantErr: "sync check_interval too short"},
		{name: "archival min_height 0", modify: func(c *MultiChainConfig) { c.Networks[0].Archival = &ArchivalCheck{MinHeight: 0} }},
		{name: "archival min_height -1", modify: func(c *MultiChainConfig) { c.Networks[0].Archival = &ArchivalCheck{MinHeight: -1} }, wantErr: "archival min_height cannot be negative"},
		{name: "http code 100", modify: func(c *MultiChainConfig) { c.Networks[0].CircuitBreaker = &CircuitBreakerConfig{HTTPCodes: []int{100}} }},
		{name: "http code 599", modify: func(c *MultiChainConfig) { c.Networks[0].CircuitBreaker = &CircuitBreakerConfig{HTTPCodes: []int{599}} }},
		{name: "http code 99", modify: func(c *MultiChainConfig) { c.Networks[0].CircuitBreaker = &CircuitBreakerConfig{HTTPCodes: []int{99}} }, wantErr: "http_codes: 99 is not an HTTP status code"},
		{name: "http code 600", modify: func(c *MultiChainConfig) { c.Networks[0].CircuitBreaker = &CircuitBreakerConfig{HTTPCodes: []int{600}} }, wantErr: "http_codes: 600 is not an HTTP status code"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validMultiChainConfig()
			tt.modify(&cfg)
			err := ValidateMultiChain(&cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// A network with a height_check section but no interval is loaded with the
// 30s default written into the struct (not only returned by the accessor).
// Pins the default assignment at multichain_loader.go:381.
func TestMultiChainLoader_HeightCheckIntervalDefaultIsStored(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		yaml        string
		want        time.Duration
		intervalSet bool
	}{
		{name: "unset becomes 30s", yaml: strings.Replace(validMultiChainYAML, "      interval: 30s\n", "", 1), want: 30 * time.Second},
		{name: "explicit value is kept", yaml: strings.Replace(validMultiChainYAML, "interval: 30s", "interval: 7s", 1), want: 7 * time.Second, intervalSet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !tt.intervalSet && strings.Contains(tt.yaml, "interval:") {
				t.Fatal("test setup: the interval line was not removed from the YAML")
			}
			loader, err := NewMultiChainLoader(writeTestConfig(t, tt.yaml), zap.NewNop())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			hc := loader.Get().Networks[0].HeightCheck
			if hc == nil {
				t.Fatal("HeightCheck is nil")
			}
			if hc.Interval != tt.want {
				t.Fatalf("HeightCheck.Interval = %v, want %v", hc.Interval, tt.want)
			}
		})
	}
}

// LogReloadResult stays silent on success, warns on ErrRestartRequired (also
// when wrapped) and logs an error for any other failure.
// Pins the `err == nil` case at multichain_loader.go:339.
func TestLogReloadResult_Levels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       error
		wantLevel zapcore.Level
		wantLogs  int
		wantMsg   string
	}{
		{name: "nil is silent", err: nil, wantLogs: 0},
		{name: "restart required warns", err: ErrRestartRequired, wantLevel: zapcore.WarnLevel, wantLogs: 1, wantMsg: "restart to apply them"},
		{name: "wrapped restart required warns", err: fmt.Errorf("reload: %w", ErrRestartRequired), wantLevel: zapcore.WarnLevel, wantLogs: 1, wantMsg: "restart to apply them"},
		{name: "other error is an error", err: errors.New("boom"), wantLevel: zapcore.ErrorLevel, wantLogs: 1, wantMsg: "keeping previous configuration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			core, logs := observer.New(zapcore.DebugLevel)
			LogReloadResult(zap.New(core), "SIGHUP", tt.err)
			entries := logs.All()
			if len(entries) != tt.wantLogs {
				t.Fatalf("got %d log entries %v, want %d", len(entries), entries, tt.wantLogs)
			}
			if tt.wantLogs == 0 {
				return
			}
			if entries[0].Level != tt.wantLevel {
				t.Errorf("level = %v, want %v", entries[0].Level, tt.wantLevel)
			}
			if !strings.Contains(entries[0].Message, tt.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", entries[0].Message, tt.wantMsg)
			}
			if got := entries[0].ContextMap()["trigger"]; got != "SIGHUP" {
				t.Errorf("trigger field = %v, want SIGHUP", got)
			}
		})
	}
}
