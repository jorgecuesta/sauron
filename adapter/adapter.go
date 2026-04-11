// Package adapter defines the chain adapter interface and configuration types
// that drive Sauron's universal health/height check engine.
//
// Native adapters (cosmos, evm, solana) are factories that produce CheckConfig
// for the shared engine. Custom adapters pass config through directly.
package adapter

// ChainAdapter is the interface that all chain-type factories implement.
// A factory inspects the network configuration and produces check configs
// that the shared engine executes.
type ChainAdapter interface {
	// Type returns the chain type name (e.g. "cosmos", "evm", "solana").
	Type() string

	// HeightCheck returns the config for determining a node's block height.
	// There is exactly one height check per network, using one protocol.
	HeightCheck(net NetworkConfig) (CheckConfig, error)

	// HealthChecks returns configs for per-endpoint liveness checks.
	// One per protocol endpoint that the node exposes.
	HealthChecks(net NetworkConfig, node NodeConfig) ([]HealthCheckConfig, error)

	// ArchivalCheck returns the config for verifying a node has historical data
	// at the given block height. The engine executes this config — if it returns
	// a valid height, the node is considered archival.
	ArchivalCheck(net NetworkConfig, minHeight int64) (CheckConfig, error)
}

// NetworkConfig is the subset of network configuration that adapters need.
// This avoids coupling adapters to the full config package.
type NetworkConfig struct {
	Name     string
	Type     string
	Interval string // e.g. "30s"
	Mode     string // Chain-specific mode (e.g. EVM: "latest", "safe", "finalized")

	// HeightCheck overrides (used by custom type, ignored by native adapters).
	HeightCheck *HeightCheckOverride

	// Protocol-specific settings from the network config.
	Endpoints []EndpointConfig
}

// HeightCheckOverride holds the operator-defined height check configuration
// for custom chain types.
type HeightCheckOverride struct {
	Protocol       string
	Method         string
	URLPath        string
	Headers        map[string]string
	Body           string
	ResponsePath   string
	ResponseFormat string // "integer", "hex", "string"
}

// EndpointConfig describes a single protocol endpoint for a network.
type EndpointConfig struct {
	Protocol string // "rest", "rpc", "grpc", "jsonrpc", "http", "websocket"
	Listen   string
}

// NodeConfig describes a single node's endpoints.
type NodeConfig struct {
	Name     string
	Network  string
	Endpoint map[string]string // protocol → URL
}
