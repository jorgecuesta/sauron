package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// What the Eye records - The archives of Barad-dûr

var (
	// Node Health & Performance Metrics

	// NodeHeight tracks the current blockchain height by node
	NodeHeight = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sauron_node_height",
			Help: "Current blockchain height by node and endpoint type",
		},
		[]string{"network", "node", "type", "source"}, // source: internal|external
	)

	// NodeLatency tracks response latency for each node
	NodeLatency = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sauron_node_latency_seconds",
			Help:    "Node response latency in seconds",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2, 5, 10},
		},
		[]string{"network", "node", "type"},
	)

	// NodeAvailable indicates if a node is reachable (1=up, 0=down)
	NodeAvailable = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sauron_node_available",
			Help: "Node availability status (1=up, 0=down)",
		},
		[]string{"network", "node", "type"},
	)

	// NodeHeightStaleness tracks time since last height update
	NodeHeightStaleness = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sauron_node_height_staleness_seconds",
			Help: "Seconds since last successful height update",
		},
		[]string{"network", "node", "type"},
	)

	// HeightCheckDuration tracks how long height checks take
	HeightCheckDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sauron_height_check_duration_seconds",
			Help:    "Duration of height check operations",
			Buckets: []float64{.1, .25, .5, 1, 2, 5, 10},
		},
		[]string{"network", "node", "type"},
	)

	// HeightCheckErrors counts failed height checks
	HeightCheckErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sauron_height_check_errors_total",
			Help: "Total number of failed height checks",
		},
		[]string{"network", "node", "type", "error_type"},
	)

	// Routing Analytics

	// RoutingSelections tracks which nodes were selected and why
	RoutingSelections = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sauron_routing_selections_total",
			Help: "Total number of routing selections by node and reason",
		},
		[]string{"network", "type", "selected_node", "reason"}, // reason: height_winner|latency_tiebreaker|only_available
	)

	// RoutingFailures tracks when routing fails
	RoutingFailures = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sauron_routing_failures_total",
			Help: "Total number of routing failures",
		},
		[]string{"network", "type", "reason"}, // reason: no_nodes|all_unhealthy|timeout
	)

	// NodeRequests tracks request distribution per node
	NodeRequests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sauron_node_requests_total",
			Help: "Total number of requests routed to each node",
		},
		[]string{"network", "node", "type", "method"},
	)

	// RoutingAlternativesConsidered tracks how many nodes were considered
	RoutingAlternativesConsidered = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sauron_routing_alternatives_considered",
			Help:    "Number of alternative nodes considered during selection",
			Buckets: []float64{1, 2, 3, 5, 10, 20, 50},
		},
		[]string{"network", "type"},
	)

	// Proxy Performance Metrics

	// ProxyRequestDuration tracks end-to-end proxy request duration
	ProxyRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sauron_proxy_request_duration_seconds",
			Help:    "Duration of proxied requests",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
		},
		[]string{"network", "node", "type", "status"},
	)

	// ProxyResponseSize tracks response sizes
	ProxyResponseSize = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "sauron_proxy_response_size_bytes",
			Help: "Size of proxy responses in bytes",
			Buckets: []float64{
				1024,       // 1KB
				10240,      // 10KB
				102400,     // 100KB
				1048576,    // 1MB
				10485760,   // 10MB
				104857600,  // 100MB
				524288000,  // 500MB
				1073741824, // 1GB
			},
		},
		[]string{"network", "type"},
	)

	// ProxyErrors tracks proxy errors by type
	ProxyErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sauron_proxy_errors_total",
			Help: "Total number of proxy errors",
		},
		[]string{"network", "node", "type", "status_code", "error_type"},
	)

	// ProxyActiveConnections tracks active proxy connections
	ProxyActiveConnections = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sauron_proxy_active_connections",
			Help: "Number of active proxy connections",
		},
		[]string{"network", "node", "type"},
	)

	// User Analytics

	// UserRequests tracks requests per user
	UserRequests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sauron_user_requests_total",
			Help: "Total number of requests per user",
		},
		[]string{"user", "network", "type", "method"},
	)

	// AuthFailures tracks authentication failures
	AuthFailures = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sauron_auth_failures_total",
			Help: "Total number of authentication failures",
		},
		[]string{"reason"}, // reason: invalid_token|missing_token|forbidden_type
	)

	// External Ring Performance

	// ExternalRingLatency tracks response time from external Sauron rings
	ExternalRingLatency = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sauron_external_ring_latency_seconds",
			Help:    "Latency of external ring queries",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2, 5},
		},
		[]string{"ring_name", "ring_url"},
	)

	// ExternalHeightDelta tracks height difference between external and internal
	ExternalHeightDelta = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sauron_external_height_delta",
			Help: "Height difference between external rings and internal nodes",
		},
		[]string{"network", "ring_name", "type"},
	)

	// ExternalRingAvailable indicates if an external ring is reachable
	ExternalRingAvailable = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sauron_external_ring_available",
			Help: "External ring availability (1=up, 0=down)",
		},
		[]string{"ring_name", "ring_url"},
	)

	// ExternalRingErrors tracks external ring query errors
	ExternalRingErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sauron_external_ring_errors_total",
			Help: "Total number of external ring errors",
		},
		[]string{"ring_name", "ring_url", "error_type"},
	)

	// External Endpoint Tracking (advertised endpoints from rings)

	// ExternalEndpointsTracked tracks total number of external endpoints discovered
	ExternalEndpointsTracked = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sauron_external_endpoints_tracked",
			Help: "Number of external endpoints currently tracked (advertised)",
		},
		[]string{"network", "type", "ring_name"},
	)

	// ExternalEndpointsValidated tracks number of validated+working endpoints
	ExternalEndpointsValidated = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sauron_external_endpoints_validated",
			Help: "Number of external endpoints validated and working",
		},
		[]string{"network", "type", "ring_name"},
	)

	// ExternalEndpointsWorking tracks number of endpoints currently working
	ExternalEndpointsWorking = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sauron_external_endpoints_working",
			Help: "Number of external endpoints currently working (not failed)",
		},
		[]string{"network", "type", "ring_name"},
	)

	// ExternalEndpointValidationAttempts tracks endpoint validation attempts
	ExternalEndpointValidationAttempts = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sauron_external_endpoint_validation_attempts_total",
			Help: "Total number of external endpoint validation attempts",
		},
		[]string{"network", "type", "ring_name", "result"}, // result: success|failure
	)

	// ExternalEndpointProxyErrors tracks 5xx errors from external endpoints
	ExternalEndpointProxyErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sauron_external_endpoint_proxy_errors_total",
			Help: "Total number of 5xx proxy errors from external endpoints",
		},
		[]string{"network", "type", "url"},
	)

	// ExternalEndpointErrorCount tracks current error count per endpoint
	ExternalEndpointErrorCount = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sauron_external_endpoint_error_count",
			Help: "Current consecutive error count for external endpoint",
		},
		[]string{"network", "type", "url"},
	)

	// ExternalEndpointRecoveries tracks successful recoveries from failed state
	ExternalEndpointRecoveries = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sauron_external_endpoint_recoveries_total",
			Help: "Total number of successful endpoint recoveries from failed state",
		},
		[]string{"network", "type", "ring_name"},
	)

	// ExternalEndpointValidationLatency tracks endpoint validation latency
	ExternalEndpointValidationLatency = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sauron_external_endpoint_validation_latency_seconds",
			Help:    "Latency of external endpoint validation checks",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2},
		},
		[]string{"network", "type", "ring_name"},
	)

	// Cache Performance

	// CacheOperations tracks cache hits/misses
	CacheOperations = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sauron_cache_operations_total",
			Help: "Total number of cache operations",
		},
		[]string{"operation", "result"}, // operation: get|set|delete, result: hit|miss|error
	)

	// CacheOperationDuration tracks cache operation latency
	CacheOperationDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sauron_cache_operation_duration_seconds",
			Help:    "Duration of cache operations",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5},
		},
		[]string{"operation"},
	)

	// System Health Metrics

	// WorkerPoolActive tracks active workers
	WorkerPoolActive = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "sauron_worker_pool_active_workers",
			Help: "Number of active workers in the pool",
		},
	)

	// WorkerPoolQueueDepth tracks queued tasks
	WorkerPoolQueueDepth = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "sauron_worker_pool_queue_depth",
			Help: "Number of tasks waiting in the worker pool queue",
		},
	)

	// WorkerTaskDuration tracks task execution time
	WorkerTaskDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sauron_worker_task_duration_seconds",
			Help:    "Duration of worker task execution",
			Buckets: []float64{.1, .25, .5, 1, 2, 5, 10},
		},
		[]string{"task_type"},
	)

	// ConfigReloads tracks configuration reload events
	ConfigReloads = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sauron_config_reloads_total",
			Help: "Total number of configuration reload attempts",
		},
		[]string{"result"}, // result: success|failure
	)

	// KEDA Autoscaling Metrics

	// KEDARequestRate tracks request rate per second for autoscaling
	KEDARequestRate = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sauron_keda_request_rate_per_second",
			Help: "Request rate per second for KEDA autoscaling",
		},
		[]string{"network", "type"},
	)

	// KEDALatencyP95 tracks 95th percentile latency
	KEDALatencyP95 = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sauron_keda_latency_p95_seconds",
			Help: "95th percentile latency for KEDA autoscaling",
		},
		[]string{"network", "type"},
	)

	// KEDALatencyP99 tracks 99th percentile latency
	KEDALatencyP99 = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sauron_keda_latency_p99_seconds",
			Help: "99th percentile latency for KEDA autoscaling",
		},
		[]string{"network", "type"},
	)

	// KEDAErrorRate tracks error rate percentage
	KEDAErrorRate = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sauron_keda_error_rate_percent",
			Help: "Error rate percentage for KEDA autoscaling",
		},
		[]string{"network", "type"},
	)

	// KEDAConnectionUtilization tracks connection pool utilization
	KEDAConnectionUtilization = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sauron_keda_connection_utilization_percent",
			Help: "Connection pool utilization percentage for KEDA autoscaling",
		},
		[]string{"type"},
	)
)
