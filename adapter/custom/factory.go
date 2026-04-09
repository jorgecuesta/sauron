// Package custom provides the custom chain adapter factory.
// It passes operator-defined height check configuration directly to the engine.
// No protocol-specific logic — the operator defines everything.
package custom

import (
	"fmt"
	"net/http"

	"sauron/adapter"
)

// Factory produces check configurations from operator-defined settings.
// This is the passthrough adapter: the config IS the check.
type Factory struct{}

// New creates a new custom adapter factory.
func New() *Factory {
	return &Factory{}
}

// Type returns "custom".
func (f *Factory) Type() string { return "custom" }

// HeightCheck converts the operator-defined HeightCheckOverride into a CheckConfig.
// Returns an error if the override is missing (required for custom type).
func (f *Factory) HeightCheck(net adapter.NetworkConfig) (adapter.CheckConfig, error) {
	if net.HeightCheck == nil {
		return adapter.CheckConfig{}, fmt.Errorf("custom: height_check configuration is required")
	}

	hc := net.HeightCheck

	if hc.Method == "" {
		return adapter.CheckConfig{}, fmt.Errorf("custom: height_check method is required")
	}
	if hc.ResponsePath == "" {
		return adapter.CheckConfig{}, fmt.Errorf("custom: height_check response_path is required")
	}

	headers := make(http.Header)
	for k, v := range hc.Headers {
		headers.Set(k, v)
	}

	format := hc.ResponseFormat
	if format == "" {
		format = "integer"
	}

	// Determine protocol from the first endpoint, or default to "http".
	protocol := "http"
	if hc.Protocol != "" {
		protocol = hc.Protocol
	}

	return adapter.CheckConfig{
		Method:         hc.Method,
		URLPath:        hc.URLPath,
		Headers:        headers,
		Body:           []byte(hc.Body),
		ResponsePath:   hc.ResponsePath,
		ResponseFormat: format,
		Protocol:       protocol,
	}, nil
}

// HealthChecks returns a basic HTTP health check for each endpoint.
// Custom nodes get a simple GET/POST liveness check — the operator's
// height check URL is used as the health check path too.
func (f *Factory) HealthChecks(net adapter.NetworkConfig, node adapter.NodeConfig) ([]adapter.HealthCheckConfig, error) {
	var checks []adapter.HealthCheckConfig

	for protocol, url := range node.Endpoint {
		if url == "" {
			continue
		}
		switch protocol {
		case "http", "jsonrpc":
			checks = append(checks, adapter.HealthCheckConfig{
				Protocol:     protocol,
				Method:       "GET",
				URLPath:      "/",
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
