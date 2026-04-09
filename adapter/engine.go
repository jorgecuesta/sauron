package adapter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"sauron/internal/jsonpath"
)

// Engine executes CheckConfig and HealthCheckConfig against nodes.
// There is ONE engine — all chain types share it .
type Engine struct {
	client *http.Client
}

// NewEngine creates an engine with the given HTTP client.
func NewEngine(client *http.Client) *Engine {
	return &Engine{client: client}
}

// CheckHeight executes a height check config against a node URL and returns the block height.
func (e *Engine) CheckHeight(ctx context.Context, cfg CheckConfig, nodeURL string) (int64, error) {
	if cfg.IsGRPC {
		return 0, fmt.Errorf("engine: gRPC height checks not yet implemented")
	}

	body, err := e.doHTTP(ctx, cfg.Method, nodeURL+cfg.URLPath, cfg.Headers, cfg.Body)
	if err != nil {
		return 0, fmt.Errorf("engine: height check request failed: %w", err)
	}

	height, err := jsonpath.ExtractInt64(body, cfg.ResponsePath)
	if err != nil {
		return 0, fmt.Errorf("engine: failed to extract height from response: %w", err)
	}

	return height, nil
}

// CheckHealth executes a health check config against a node URL and returns nil if healthy.
func (e *Engine) CheckHealth(ctx context.Context, cfg HealthCheckConfig, nodeURL string) error {
	if cfg.IsGRPC {
		return fmt.Errorf("engine: gRPC health checks not yet implemented")
	}
	if cfg.IsWebSocket {
		return fmt.Errorf("engine: WebSocket health checks not yet implemented")
	}

	expectStatus := cfg.ExpectStatus
	if expectStatus == 0 {
		expectStatus = http.StatusOK
	}

	url := nodeURL + cfg.URLPath
	var bodyReader io.Reader
	if len(cfg.Body) > 0 {
		bodyReader = strings.NewReader(string(cfg.Body))
	}

	req, err := http.NewRequestWithContext(ctx, cfg.Method, url, bodyReader)
	if err != nil {
		return fmt.Errorf("engine: health check request creation failed: %w", err)
	}
	for k, vs := range cfg.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("engine: health check request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain body to allow connection reuse.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != expectStatus {
		return fmt.Errorf("engine: health check got status %d, want %d", resp.StatusCode, expectStatus)
	}

	return nil
}

func (e *Engine) doHTTP(ctx context.Context, method, url string, headers http.Header, body []byte) ([]byte, error) {
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = strings.NewReader(string(body))
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Drain and discard to allow connection reuse.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}
