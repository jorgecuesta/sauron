package status

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"sauron/config"
	"sauron/selector"
	"sauron/storage"
	"time"

	"go.uber.org/zap"
)

// writeHandlerConfig writes a YAML config file and returns its path.
// hasInternals controls whether internal nodes are included.
func writeHandlerConfig(t *testing.T, hasInternals bool) string {
	t.Helper()

	internals := ""
	if hasInternals {
		internals = `
internals:
  - name: node-1
    api: "https://node1.example.com"
    network: pocket
`
	} else {
		// Need at least one external when no internals
		internals = `
externals:
  - name: ext-ring
    rings:
      - "https://ring1.example.com"
`
	}

	content := `
api: true
rpc: false
grpc: false
auth: false
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 30s
networks:
  - name: pocket
    api_listen: ":8080"
    api: "https://api.pocket.example.com"
` + internals

	f, err := os.CreateTemp("", "sauron-handler-test-*.yaml")
	if err != nil {
		t.Fatalf("writeHandlerConfig: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("writeHandlerConfig write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("writeHandlerConfig close: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	return f.Name()
}

// buildHandler creates a Handler+HeightStore for handler tests.
func buildHandler(t *testing.T, hasInternals bool) (*Handler, *storage.HeightStore) {
	t.Helper()
	logger := zap.NewNop()
	path := writeHandlerConfig(t, hasInternals)
	loader, err := config.NewLoader(path, logger)
	if err != nil {
		t.Fatalf("buildHandler NewLoader: %v", err)
	}
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	sel := selector.NewSelector(heightStore, endpointStore, loader, logger)
	h := NewHandler(sel, loader, logger)
	t.Cleanup(h.Shutdown)
	return h, heightStore
}

func TestHealth_Returns200(t *testing.T) {
	t.Parallel()
	h, _ := buildHandler(t, true)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "OK") {
		t.Errorf("expected body 'OK', got %q", w.Body.String())
	}
}

func TestReady_WithInternals(t *testing.T) {
	t.Parallel()
	h, _ := buildHandler(t, true)

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	w := httptest.NewRecorder()
	h.handleReady(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with internals configured, got %d", w.Code)
	}
}

func TestReady_NoInternals(t *testing.T) {
	t.Parallel()
	h, _ := buildHandler(t, false)

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	w := httptest.NewRecorder()
	h.handleReady(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 with no internals, got %d", w.Code)
	}
}

func TestMetrics_Returns200(t *testing.T) {
	t.Parallel()
	h, _ := buildHandler(t, true)

	mux := http.NewServeMux()
	h.SetupRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for /metrics, got %d", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Error("expected non-empty metrics body")
	}
}

func TestStatus_ReturnsHeight(t *testing.T) {
	t.Parallel()
	h, heightStore := buildHandler(t, true)

	// Populate the height store so the selector has data
	heightStore.Update("pocket", "node-1", "api", 12345, 10*time.Millisecond, "internal")

	mux := http.NewServeMux()
	h.SetupRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/pocket/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	var resp StatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response JSON: %v", err)
	}

	if resp.Height != 12345 {
		t.Errorf("expected height 12345, got %d", resp.Height)
	}
}

func TestStatus_UnknownNetwork(t *testing.T) {
	t.Parallel()
	h, _ := buildHandler(t, true)

	mux := http.NewServeMux()
	h.SetupRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/unknown-net/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown network, got %d", w.Code)
	}
}
