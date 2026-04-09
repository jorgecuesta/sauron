package adapter

import "net/http"

// CheckConfig describes how to perform a height check against a node.
// The engine executes this config — adapters produce it. This is the
// single representation that all chain types share (V2 rule: one path).
type CheckConfig struct {
	// Method is the HTTP method: GET or POST.
	Method string
	// URLPath is appended to the node's base URL (e.g. "/status", "/graphql").
	URLPath string
	// Headers to include in the request.
	Headers http.Header
	// Body is the request body for POST requests (JSON-RPC, GraphQL, etc).
	Body []byte
	// ResponsePath is the jsonpath expression to extract the height value
	// (e.g. ".result.sync_info.latest_block_height").
	ResponsePath string
	// ResponseFormat indicates how to parse the extracted value:
	//   "integer" — numeric or string-encoded integer
	//   "hex"     — hex-encoded integer (0x prefix, e.g. EVM)
	//   "string"  — string-encoded integer (same as integer, kept for clarity)
	ResponseFormat string

	// Protocol identifies which node endpoint URL to use (e.g. "rpc", "jsonrpc").
	Protocol string

	// IsGRPC indicates this check uses gRPC instead of HTTP.
	// When true, the engine uses a gRPC client instead of HTTP.
	IsGRPC bool
	// GRPCService is the full gRPC service path (e.g. "/cosmos.base.tendermint.v1beta1.Service/GetLatestBlock").
	GRPCService string
	// GRPCMethod is the gRPC method within the service.
	GRPCMethod string
}

// HealthCheckConfig describes how to verify that a specific protocol
// endpoint on a node is alive and responding.
type HealthCheckConfig struct {
	// Protocol identifies which endpoint to check (e.g. "rest", "rpc", "grpc").
	Protocol string
	// Method is the HTTP method (GET or POST). Ignored for gRPC.
	Method string
	// URLPath is appended to the node's base URL.
	URLPath string
	// Headers for the request.
	Headers http.Header
	// Body for POST requests.
	Body []byte
	// ExpectStatus is the expected HTTP status code (default 200).
	ExpectStatus int

	// IsGRPC indicates this is a gRPC health check.
	IsGRPC bool
	// IsWebSocket indicates this is a WebSocket connectivity check.
	IsWebSocket bool
}
