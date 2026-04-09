package storage

import (
	"fmt"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
)

// ArchivalStatus tracks whether a node has archival data.
type ArchivalStatus struct {
	IsArchival bool
	LastCheck  time.Time
}

// ArchivalStore tracks which nodes have been verified to hold archival
// (historical) block data. The checker periodically queries each node
// for a block at the configured min_height to confirm archival status.
//
// Thread-safe via xsync.Map.
type ArchivalStore struct {
	data *xsync.Map[string, *ArchivalStatus]
}

// NewArchivalStore creates a new archival store.
func NewArchivalStore() *ArchivalStore {
	return &ArchivalStore{
		data: xsync.NewMap[string, *ArchivalStatus](),
	}
}

// archivalKey creates a unique key for a node's archival status.
func archivalKey(network, node string) string {
	return fmt.Sprintf("%s:%s", network, node)
}

// SetArchival marks a node as having archival data.
func (s *ArchivalStore) SetArchival(network, node string) {
	key := archivalKey(network, node)
	s.data.Store(key, &ArchivalStatus{
		IsArchival: true,
		LastCheck:  time.Now(),
	})
}

// SetNotArchival marks a node as NOT having archival data.
func (s *ArchivalStore) SetNotArchival(network, node string) {
	key := archivalKey(network, node)
	s.data.Store(key, &ArchivalStatus{
		IsArchival: false,
		LastCheck:  time.Now(),
	})
}

// IsArchival returns whether a node has been verified as archival.
// Returns false if the node has never been checked.
func (s *ArchivalStore) IsArchival(network, node string) bool {
	key := archivalKey(network, node)
	status, ok := s.data.Load(key)
	if !ok {
		return false
	}
	return status.IsArchival
}

// GetStatus returns the full archival status. Returns nil if never checked.
func (s *ArchivalStore) GetStatus(network, node string) *ArchivalStatus {
	key := archivalKey(network, node)
	status, ok := s.data.Load(key)
	if !ok {
		return nil
	}
	cp := *status
	return &cp
}
