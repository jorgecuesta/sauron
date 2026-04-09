package storage

import (
	"time"

	"github.com/puzpuzpuz/xsync/v4"
)

// OracleHeight holds the reference height from an external oracle source.
type OracleHeight struct {
	Height    int64
	Source    string    // Oracle URL that provided this height.
	Timestamp time.Time // When this height was last updated.
}

// OracleStore tracks reference block heights from external oracle sources,
// one per network. Used by the sync filter to detect nodes that have
// drifted too far from the canonical chain height.
//
// Thread-safe via xsync.Map (lock-free reads).
type OracleStore struct {
	data *xsync.Map[string, *OracleHeight]
}

// NewOracleStore creates a new oracle height store.
func NewOracleStore() *OracleStore {
	return &OracleStore{
		data: xsync.NewMap[string, *OracleHeight](),
	}
}

// Update stores or updates the oracle height for a network.
// If the new height is higher than the current one, it replaces it.
// If the new height is lower or equal, it is ignored (oracles should
// only move forward).
func (s *OracleStore) Update(network string, height int64, source string) {
	existing, loaded := s.data.Load(network)
	if loaded && existing.Height >= height {
		return
	}
	s.data.Store(network, &OracleHeight{
		Height:    height,
		Source:    source,
		Timestamp: time.Now(),
	})
}

// Get returns the oracle height for a network.
// Returns 0, false if no oracle height has been recorded.
func (s *OracleStore) Get(network string) (int64, bool) {
	h, ok := s.data.Load(network)
	if !ok {
		return 0, false
	}
	return h.Height, true
}

// GetFull returns the full OracleHeight for a network.
// Returns nil if no oracle height has been recorded.
func (s *OracleStore) GetFull(network string) *OracleHeight {
	h, ok := s.data.Load(network)
	if !ok {
		return nil
	}
	// Return a copy.
	cp := *h
	return &cp
}

// All returns a snapshot of all oracle heights.
func (s *OracleStore) All() map[string]OracleHeight {
	result := make(map[string]OracleHeight)
	s.data.Range(func(network string, val *OracleHeight) bool {
		result[network] = *val
		return true
	})
	return result
}
