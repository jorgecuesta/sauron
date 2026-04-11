package adapter

import (
	"testing"
)

// stubAdapter implements ChainAdapter for testing.
type stubAdapter struct {
	typeName string
}

func (s *stubAdapter) Type() string { return s.typeName }
func (s *stubAdapter) HeightCheck(_ NetworkConfig) (CheckConfig, error) {
	return CheckConfig{Protocol: "rpc"}, nil
}
func (s *stubAdapter) HealthChecks(_ NetworkConfig, _ NodeConfig) ([]HealthCheckConfig, error) {
	return nil, nil
}
func (s *stubAdapter) ArchivalCheck(_ NetworkConfig, _ int64) (CheckConfig, error) {
	return CheckConfig{Protocol: "rpc"}, nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	a := &stubAdapter{typeName: "cosmos"}

	if err := r.Register(a); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := r.Get("cosmos")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Type() != "cosmos" {
		t.Fatalf("got type %q, want %q", got.Type(), "cosmos")
	}
}

func TestRegistry_DuplicateRegister(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	a := &stubAdapter{typeName: "evm"}

	if err := r.Register(a); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := r.Register(a); err == nil {
		t.Fatal("expected error for duplicate registration")
	}
}

func TestRegistry_GetUnknown(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	_, err := r.Get("unknown")
	if err == nil {
		t.Fatal("expected error for unknown chain type")
	}
}

func TestRegistry_Types(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	_ = r.Register(&stubAdapter{typeName: "cosmos"})
	_ = r.Register(&stubAdapter{typeName: "evm"})

	types := r.Types()
	if len(types) != 2 {
		t.Fatalf("got %d types, want 2", len(types))
	}

	typeSet := make(map[string]bool)
	for _, tp := range types {
		typeSet[tp] = true
	}
	if !typeSet["cosmos"] || !typeSet["evm"] {
		t.Fatalf("missing expected types: %v", types)
	}
}

func TestRegistry_EmptyTypes(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	types := r.Types()
	if len(types) != 0 {
		t.Fatalf("expected empty types, got %v", types)
	}
}
