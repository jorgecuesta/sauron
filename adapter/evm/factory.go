// Package evm provides the EVM chain adapter factory.
// It produces CheckConfig and HealthCheckConfig for Ethereum-compatible chains.
package evm

import (
	"fmt"
	"net/http"

	"sauron/adapter"
)

// Supported EVM block tag modes for eth_blockNumber.
const (
	ModeLatest    = "latest"
	ModeSafe      = "safe"
	ModeFinalized = "finalized"
)

// Factory produces check configurations for EVM-compatible chains.
type Factory struct{}

// New creates a new EVM adapter factory.
func New() *Factory {
	return &Factory{}
}

// Type returns "evm".
func (f *Factory) Type() string { return "evm" }

// HeightCheck returns the CheckConfig for determining block height via eth_blockNumber.
// The mode (latest, safe, finalized) determines which block tag is used.
func (f *Factory) HeightCheck(net adapter.NetworkConfig) (adapter.CheckConfig, error) {
	mode := resolveMode(net)

	body, responsePath, err := heightCheckBody(mode)
	if err != nil {
		return adapter.CheckConfig{}, err
	}

	return adapter.CheckConfig{
		Method:         "POST",
		URLPath:        "/",
		Headers:        http.Header{"Content-Type": {"application/json"}},
		Body:           body,
		ResponsePath:   responsePath,
		ResponseFormat: "hex",
		Protocol:       "jsonrpc",
	}, nil
}

// HealthChecks returns health check configs for each protocol endpoint a node exposes.
func (f *Factory) HealthChecks(net adapter.NetworkConfig, node adapter.NodeConfig) ([]adapter.HealthCheckConfig, error) {
	var checks []adapter.HealthCheckConfig

	for protocol, url := range node.Endpoint {
		if url == "" {
			continue
		}
		switch protocol {
		case "jsonrpc":
			checks = append(checks, adapter.HealthCheckConfig{
				Protocol:     "jsonrpc",
				Method:       "POST",
				URLPath:      "/",
				Headers:      http.Header{"Content-Type": {"application/json"}},
				Body:         []byte(`{"jsonrpc":"2.0","method":"net_version","params":[],"id":1}`),
				ExpectStatus: http.StatusOK,
			})
		case "websocket":
			checks = append(checks, adapter.HealthCheckConfig{
				Protocol:    "websocket",
				IsWebSocket: true,
			})
		}
	}

	return checks, nil
}

// resolveMode returns the EVM block tag mode, defaulting to "latest".
func resolveMode(net adapter.NetworkConfig) string {
	if net.Mode != "" {
		return net.Mode
	}
	return ModeLatest
}

// heightCheckBody returns the JSON-RPC request body and response path for the given mode.
//
//   - "latest": uses eth_blockNumber (returns hex height directly in .result)
//   - "safe"/"finalized": uses eth_getBlockByNumber with the block tag
//     (returns block object, height in .result.number)
func heightCheckBody(mode string) (body []byte, responsePath string, err error) {
	switch mode {
	case ModeLatest:
		return []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`),
			".result", nil
	case ModeSafe:
		return []byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["safe",false],"id":1}`),
			".result.number", nil
	case ModeFinalized:
		return []byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["finalized",false],"id":1}`),
			".result.number", nil
	default:
		return nil, "", fmt.Errorf("evm: unsupported mode %q (must be latest, safe, or finalized)", mode)
	}
}
