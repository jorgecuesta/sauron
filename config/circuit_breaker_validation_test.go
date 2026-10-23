package config

import (
	"strings"
	"testing"
	"time"
)

// TestValidateCircuitBreaker: a circuit_breaker section is validated when the
// config is written, so a typo is an error at load instead of a silent no-op.
func TestValidateCircuitBreaker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cb      CircuitBreakerConfig
		wantErr string // empty: valid
	}{
		{name: "typical", cb: CircuitBreakerConfig{HTTPCodes: []int{502, 503}, GRPCCodes: []string{"unavailable", "internal"}, Threshold: 3, RetryAttempts: 2, RetryBackoff: 100 * time.Millisecond}},
		{name: "empty section", cb: CircuitBreakerConfig{}},
		{name: "grpc code names are case and separator insensitive", cb: CircuitBreakerConfig{GRPCCodes: []string{"RESOURCE_EXHAUSTED", "ResourceExhausted", "deadline_exceeded", " unavailable "}}},
		{name: "unknown grpc code", cb: CircuitBreakerConfig{GRPCCodes: []string{"unavailable", "unavailible"}}, wantErr: `unknown gRPC code "unavailible"`},
		{name: "empty grpc code", cb: CircuitBreakerConfig{GRPCCodes: []string{""}}, wantErr: `unknown gRPC code ""`},
		{name: "ok is not a failure code", cb: CircuitBreakerConfig{GRPCCodes: []string{"ok"}}, wantErr: `gRPC code "ok" cannot count as a failure`},
		{name: "http code below 100", cb: CircuitBreakerConfig{HTTPCodes: []int{99}}, wantErr: "http_codes: 99 is not an HTTP status code"},
		{name: "http code above 599", cb: CircuitBreakerConfig{HTTPCodes: []int{600}}, wantErr: "http_codes: 600 is not an HTTP status code"},
		{name: "negative threshold", cb: CircuitBreakerConfig{Threshold: -1}, wantErr: "threshold cannot be negative"},
		{name: "negative retry attempts", cb: CircuitBreakerConfig{RetryAttempts: -1}, wantErr: "retry_attempts cannot be negative"},
		{name: "negative retry backoff", cb: CircuitBreakerConfig{RetryBackoff: -time.Second}, wantErr: "retry_backoff cannot be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validMultiChainConfig()
			cb := tt.cb
			cfg.Networks[0].CircuitBreaker = &cb
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
			if !strings.Contains(err.Error(), "pocket") {
				t.Errorf("err = %v, want it to name the network", err)
			}
		})
	}
}
