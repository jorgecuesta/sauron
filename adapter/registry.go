package adapter

import (
	"fmt"
	"sync"
)

// Registry maps chain type names to their adapter factories.
// It is safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]ChainAdapter
}

// NewRegistry creates an empty adapter registry.
func NewRegistry() *Registry {
	return &Registry{
		adapters: make(map[string]ChainAdapter),
	}
}

// Register adds an adapter for a chain type. Returns an error if the
// type is already registered.
func (r *Registry) Register(adapter ChainAdapter) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := adapter.Type()
	if _, exists := r.adapters[name]; exists {
		return fmt.Errorf("adapter: type %q already registered", name)
	}
	r.adapters[name] = adapter
	return nil
}

// Get returns the adapter for the given chain type, or an error if not found.
func (r *Registry) Get(chainType string) (ChainAdapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	a, ok := r.adapters[chainType]
	if !ok {
		return nil, fmt.Errorf("adapter: unknown chain type %q", chainType)
	}
	return a, nil
}

// Types returns all registered chain type names.
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	types := make([]string, 0, len(r.adapters))
	for t := range r.adapters {
		types = append(types, t)
	}
	return types
}
