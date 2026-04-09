package storage

import (
	"fmt"
	"sort"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
)

// EndpointHealth tracks the health status of a single node:protocol endpoint.
type EndpointHealth struct {
	Healthy   bool
	LastCheck time.Time
	LastError string // Empty if healthy.
}

// HealthStore tracks per-node per-protocol health status.
// A node can have good height but a broken gRPC endpoint — HealthStore
// captures this so the selector can filter by protocol availability.
//
// Thread-safe via xsync.Map (lock-free reads).
type HealthStore struct {
	data *xsync.Map[string, *EndpointHealth]
}

// NewHealthStore creates a new health store.
func NewHealthStore() *HealthStore {
	return &HealthStore{
		data: xsync.NewMap[string, *EndpointHealth](),
	}
}

// healthKey creates a unique key for a node endpoint.
// Format: "network:node:protocol"
func healthKey(network, node, protocol string) string {
	return fmt.Sprintf("%s:%s:%s", network, node, protocol)
}

// SetHealthy marks a node's protocol endpoint as healthy.
func (s *HealthStore) SetHealthy(network, node, protocol string) {
	key := healthKey(network, node, protocol)
	s.data.Store(key, &EndpointHealth{
		Healthy:   true,
		LastCheck: time.Now(),
	})
}

// SetUnhealthy marks a node's protocol endpoint as unhealthy with the given error.
func (s *HealthStore) SetUnhealthy(network, node, protocol, errMsg string) {
	key := healthKey(network, node, protocol)
	s.data.Store(key, &EndpointHealth{
		Healthy:   false,
		LastCheck: time.Now(),
		LastError: errMsg,
	})
}

// IsHealthy returns whether a node's protocol endpoint is healthy.
// Returns false if the endpoint has never been checked.
func (s *HealthStore) IsHealthy(network, node, protocol string) bool {
	key := healthKey(network, node, protocol)
	h, ok := s.data.Load(key)
	if !ok {
		return false
	}
	return h.Healthy
}

// GetHealth returns the full health status for a node's protocol endpoint.
// Returns nil if the endpoint has never been checked.
func (s *HealthStore) GetHealth(network, node, protocol string) *EndpointHealth {
	key := healthKey(network, node, protocol)
	h, ok := s.data.Load(key)
	if !ok {
		return nil
	}
	// Return a copy to prevent mutation.
	cp := *h
	return &cp
}

// HealthyNodes returns the names of all healthy nodes for a given network and protocol,
// sorted alphabetically for deterministic selection.
func (s *HealthStore) HealthyNodes(network, protocol string) []string {
	prefix := network + ":"
	suffix := ":" + protocol

	var nodes []string
	s.data.Range(func(key string, val *EndpointHealth) bool {
		if len(key) > len(prefix)+len(suffix) &&
			key[:len(prefix)] == prefix &&
			key[len(key)-len(suffix):] == suffix &&
			val.Healthy {
			// Extract node name from "network:node:protocol".
			node := key[len(prefix) : len(key)-len(suffix)]
			nodes = append(nodes, node)
		}
		return true
	})

	sort.Strings(nodes)
	return nodes
}

// AllHealth returns a snapshot of all health statuses.
// Used by the status API for debugging.
func (s *HealthStore) AllHealth() map[string]EndpointHealth {
	result := make(map[string]EndpointHealth)
	s.data.Range(func(key string, val *EndpointHealth) bool {
		result[key] = *val
		return true
	})
	return result
}

// Clear removes all health entries. Used in tests.
func (s *HealthStore) Clear() {
	s.data.Range(func(key string, _ *EndpointHealth) bool {
		s.data.Delete(key)
		return true
	})
}
