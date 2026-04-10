package storage

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
)

// newNopLogger returns a no-op zap logger for use in tests.
func newNopLogger() *zap.Logger {
	return zap.NewNop()
}

// TestNewCache_Disabled_EmptyURI verifies that an empty URI creates a disabled cache.
func TestNewCache_Disabled_EmptyURI(t *testing.T) {
	t.Parallel()
	c := NewCache("", newNopLogger())
	if c == nil {
		t.Fatal("expected non-nil Cache")
	}
	if c.IsEnabled() {
		t.Error("expected IsEnabled()=false for empty URI")
	}
	if c.client != nil {
		t.Error("expected nil client for disabled cache")
	}
}

// TestNewCache_Disabled_InvalidURI verifies that an unparseable URI creates a disabled cache.
func TestNewCache_Disabled_InvalidURI(t *testing.T) {
	t.Parallel()
	// "://not-valid" cannot be parsed as a Redis URL
	c := NewCache("://not-valid", newNopLogger())
	if c == nil {
		t.Fatal("expected non-nil Cache even for invalid URI")
	}
	if c.IsEnabled() {
		t.Error("expected IsEnabled()=false for invalid URI")
	}
}

// TestIsEnabled_False verifies the zero state of a disabled cache.
func TestIsEnabled_False(t *testing.T) {
	t.Parallel()
	c := &Cache{client: nil, logger: newNopLogger()}
	if c.IsEnabled() {
		t.Error("expected IsEnabled()=false when client is nil")
	}
}

// TestSetHeight_Disabled_NoPanic verifies that SetHeight on a disabled cache
// is a no-op and does not panic.
func TestSetHeight_Disabled_NoPanic(t *testing.T) {
	t.Parallel()
	c := NewCache("", newNopLogger())

	// Should return without panic or error
	c.SetHeight(context.Background(), "pocket", "node-1", "rest", 12345, 30*time.Second)
}

// TestSetHeight_Disabled_VariousInputs verifies no panic across edge-case inputs.
func TestSetHeight_Disabled_VariousInputs(t *testing.T) {
	t.Parallel()
	c := NewCache("", newNopLogger())

	cases := []struct {
		network      string
		node         string
		endpointType string
		height       int64
		ttl          time.Duration
	}{
		{"", "", "", 0, 0},
		{"pocket", "node-1", "rpc", -1, time.Second},
		{"pocket", "node-2", "grpc", 0, 0},
		{"very-long-network-name", "very-long-node-name", "rest", 9999999999, 24 * time.Hour},
		{"net", "node", "type", 1<<62 - 1, time.Minute}, // near max int64
	}

	for _, tc := range cases {
		// Must not panic
		c.SetHeight(context.Background(), tc.network, tc.node, tc.endpointType, tc.height, tc.ttl)
	}
}

// TestSetLatency_Disabled_NoPanic verifies that SetLatency on a disabled cache
// is a no-op and does not panic.
func TestSetLatency_Disabled_NoPanic(t *testing.T) {
	t.Parallel()
	c := NewCache("", newNopLogger())

	c.SetLatency(context.Background(), "pocket", "node-1", "rest", 15*time.Millisecond, 30*time.Second)
}

// TestSetLatency_Disabled_VariousInputs verifies no panic across edge-case inputs.
func TestSetLatency_Disabled_VariousInputs(t *testing.T) {
	t.Parallel()
	c := NewCache("", newNopLogger())

	cases := []struct {
		network      string
		node         string
		endpointType string
		latency      time.Duration
		ttl          time.Duration
	}{
		{"", "", "", 0, 0},
		{"pocket", "node-1", "rpc", -1 * time.Millisecond, time.Second},
		{"pocket", "node-2", "grpc", 0, 0},
		{"net", "node", "rest", 500 * time.Millisecond, time.Hour},
		{"net", "node", "rpc", time.Duration(1<<62 - 1), time.Minute},
	}

	for _, tc := range cases {
		c.SetLatency(context.Background(), tc.network, tc.node, tc.endpointType, tc.latency, tc.ttl)
	}
}

// TestClose_Disabled_NoPanic verifies that Close on a disabled cache is a no-op.
func TestClose_Disabled_NoPanic(t *testing.T) {
	t.Parallel()
	c := NewCache("", newNopLogger())

	if err := c.Close(); err != nil {
		t.Errorf("expected nil error from Close on disabled cache, got %v", err)
	}
}

// TestClose_Disabled_Idempotent verifies that calling Close() multiple times
// on a disabled cache does not panic and always returns nil.
func TestClose_Disabled_Idempotent(t *testing.T) {
	t.Parallel()
	c := NewCache("", newNopLogger())

	for i := 0; i < 3; i++ {
		if err := c.Close(); err != nil {
			t.Errorf("Close() call %d: expected nil, got %v", i+1, err)
		}
	}
}

// TestClose_NilClient_DirectConstruct verifies Close() on a manually constructed
// Cache with a nil client (covers the nil guard branch directly).
func TestClose_NilClient_DirectConstruct(t *testing.T) {
	t.Parallel()
	c := &Cache{client: nil, logger: newNopLogger()}

	if err := c.Close(); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

// TestSetHeight_CancelledContext_NoPanic verifies that a cancelled context passed
// to SetHeight on a disabled cache does not panic.
func TestSetHeight_CancelledContext_NoPanic(t *testing.T) {
	t.Parallel()
	c := NewCache("", newNopLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Should not panic — disabled cache ignores context entirely
	c.SetHeight(ctx, "pocket", "node-1", "rest", 100, time.Second)
}

// TestSetLatency_CancelledContext_NoPanic verifies that a cancelled context passed
// to SetLatency on a disabled cache does not panic.
func TestSetLatency_CancelledContext_NoPanic(t *testing.T) {
	t.Parallel()
	c := NewCache("", newNopLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	c.SetLatency(ctx, "pocket", "node-1", "rest", 5*time.Millisecond, time.Second)
}

// TestNewCache_Disabled_ReturnsCorrectFields verifies that the logger field is
// properly set even in the disabled path.
func TestNewCache_Disabled_ReturnsCorrectFields(t *testing.T) {
	t.Parallel()
	logger := newNopLogger()
	c := NewCache("", logger)

	if c.logger == nil {
		t.Error("expected non-nil logger in disabled Cache")
	}
}
