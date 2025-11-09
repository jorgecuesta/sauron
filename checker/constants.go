package checker

import "time"

// HTTP client connection pool constants
const (
	// HTTPMaxIdleConns is the maximum number of idle connections across all hosts
	HTTPMaxIdleConns = 100
	// HTTPMaxIdleConnsPerHost is the maximum number of idle connections per host
	HTTPMaxIdleConnsPerHost = 100
	// HTTPMaxConnsPerHost is the maximum total connections per host (0 = unlimited)
	HTTPMaxConnsPerHost = 0
	// HTTPIdleConnTimeout is how long idle connections are kept alive
	HTTPIdleConnTimeout = 90 * time.Second

	// ExternalHTTPMaxIdleConns is the connection pool size for external ring checks
	ExternalHTTPMaxIdleConns = 50
	// ExternalHTTPMaxIdleConnsPerHost is the per-host pool size for external rings
	ExternalHTTPMaxIdleConnsPerHost = 50
)
