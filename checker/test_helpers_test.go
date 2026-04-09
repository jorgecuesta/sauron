package checker

import (
	"github.com/alitto/pond/v2"
)

// newTestPool creates a small worker pool for tests.
func newTestPool() pond.Pool {
	return pond.NewPool(10)
}

// drainTestPool waits for all submitted tasks to complete.
func drainTestPool(p pond.Pool) {
	p.StopAndWait()
}
