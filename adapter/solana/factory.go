// Package solana provides the Solana chain adapter factory.
// It produces CheckConfig and HealthCheckConfig for Solana nodes.
package solana

import (
	"fmt"
	"net/http"

	"sauron/adapter"
)

// Supported Solana commitment levels.
const (
	CommitmentProcessed = "processed"
	CommitmentConfirmed = "confirmed"
	CommitmentFinalized = "finalized"
)

// Supported height methods.
const (
	MethodGetSlot        = "getSlot"
	MethodGetBlockHeight = "getBlockHeight"
)

// Factory produces check configurations for Solana chains.
type Factory struct{}

// New creates a new Solana adapter factory.
func New() *Factory {
	return &Factory{}
}

// Type returns "solana".
func (f *Factory) Type() string { return "solana" }

// HeightCheck returns the CheckConfig for determining block height.
// Mode determines the commitment level (default: finalized).
// HeightCheck.Protocol determines the method: "getSlot" or "getBlockHeight"
// (default: getSlot).
func (f *Factory) HeightCheck(net adapter.NetworkConfig) (adapter.CheckConfig, error) {
	commitment := resolveCommitment(net)
	method := resolveMethod(net)

	body, err := heightCheckBody(method, commitment)
	if err != nil {
		return adapter.CheckConfig{}, err
	}

	return adapter.CheckConfig{
		Method:         "POST",
		URLPath:        "/",
		Headers:        http.Header{"Content-Type": {"application/json"}},
		Body:           body,
		ResponsePath:   ".result",
		ResponseFormat: "integer",
		Protocol:       "jsonrpc",
	}, nil
}

// HealthChecks returns health check configs for each protocol endpoint.
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
				Body:         []byte(`{"jsonrpc":"2.0","method":"getHealth","params":[],"id":1}`),
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

// resolveCommitment returns the Solana commitment level, defaulting to "finalized".
func resolveCommitment(net adapter.NetworkConfig) string {
	if net.Mode != "" {
		return net.Mode
	}
	return CommitmentFinalized
}

// resolveMethod returns the Solana height method.
// Uses HeightCheck.Protocol if set, defaults to "getSlot".
func resolveMethod(net adapter.NetworkConfig) string {
	if net.HeightCheck != nil && net.HeightCheck.Protocol != "" {
		return net.HeightCheck.Protocol
	}
	return MethodGetSlot
}

func heightCheckBody(method, commitment string) ([]byte, error) {
	switch method {
	case MethodGetSlot:
		return []byte(fmt.Sprintf(
			`{"jsonrpc":"2.0","method":"getSlot","params":[{"commitment":"%s"}],"id":1}`,
			commitment,
		)), nil
	case MethodGetBlockHeight:
		return []byte(fmt.Sprintf(
			`{"jsonrpc":"2.0","method":"getBlockHeight","params":[{"commitment":"%s"}],"id":1}`,
			commitment,
		)), nil
	default:
		return nil, fmt.Errorf("solana: unsupported method %q (must be getSlot or getBlockHeight)", method)
	}
}
