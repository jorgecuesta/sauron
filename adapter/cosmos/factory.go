// Package cosmos provides the Cosmos SDK chain adapter factory.
// It produces CheckConfig and HealthCheckConfig for the shared engine.
package cosmos

import (
	"fmt"
	"net/http"

	"sauron/adapter"
)

// Factory produces check configurations for Cosmos SDK chains.
type Factory struct{}

// New creates a new Cosmos adapter factory.
func New() *Factory {
	return &Factory{}
}

// Type returns "cosmos".
func (f *Factory) Type() string { return "cosmos" }

// HeightCheck returns the CheckConfig for determining block height.
// Cosmos supports height checks via three protocols: rpc, rest, and grpc.
func (f *Factory) HeightCheck(net adapter.NetworkConfig) (adapter.CheckConfig, error) {
	protocol := heightCheckProtocol(net)

	switch protocol {
	case "rpc":
		return adapter.CheckConfig{
			Method:         "GET",
			URLPath:        "/status",
			ResponsePath:   ".result.sync_info.latest_block_height",
			ResponseFormat: "string",
			Protocol:       "rpc",
		}, nil
	case "rest":
		// Uses sdk_block.header.height (preferred) with fallback to block.header.height.
		// The engine's jsonpath will try sdk_block first; the caller should handle fallback
		// if needed. For now we use sdk_block which is the standard Cosmos SDK response.
		return adapter.CheckConfig{
			Method:         "GET",
			URLPath:        "/cosmos/base/tendermint/v1beta1/blocks/latest",
			ResponsePath:   ".sdk_block.header.height",
			ResponseFormat: "string",
			Protocol:       "rest",
		}, nil
	case "grpc":
		return adapter.CheckConfig{
			Protocol:    "grpc",
			IsGRPC:      true,
			GRPCService: "cosmos.base.tendermint.v1beta1.Service",
			GRPCMethod:  "GetLatestBlock",
		}, nil
	default:
		return adapter.CheckConfig{}, fmt.Errorf("cosmos: unsupported height check protocol %q", protocol)
	}
}

// HealthChecks returns health check configs for each protocol endpoint a node exposes.
func (f *Factory) HealthChecks(net adapter.NetworkConfig, node adapter.NodeConfig) ([]adapter.HealthCheckConfig, error) {
	var checks []adapter.HealthCheckConfig

	for protocol, url := range node.Endpoint {
		if url == "" {
			continue
		}
		switch protocol {
		case "rest":
			checks = append(checks, adapter.HealthCheckConfig{
				Protocol:     "rest",
				Method:       "GET",
				URLPath:      "/cosmos/base/tendermint/v1beta1/blocks/latest",
				ExpectStatus: http.StatusOK,
			})
		case "rpc":
			checks = append(checks, adapter.HealthCheckConfig{
				Protocol:     "rpc",
				Method:       "GET",
				URLPath:      "/status",
				ExpectStatus: http.StatusOK,
			})
			// WebSocket health check for RPC endpoints.
			checks = append(checks, adapter.HealthCheckConfig{
				Protocol:    "websocket",
				IsWebSocket: true,
			})
		case "grpc":
			checks = append(checks, adapter.HealthCheckConfig{
				Protocol: "grpc",
				IsGRPC:   true,
			})
		}
	}

	return checks, nil
}

// heightCheckProtocol determines which protocol to use for height checks.
// Uses the network config's HeightCheck.Protocol if set, defaults to "rpc".
func heightCheckProtocol(net adapter.NetworkConfig) string {
	if net.HeightCheck != nil && net.HeightCheck.Protocol != "" {
		return net.HeightCheck.Protocol
	}
	return "rpc"
}
