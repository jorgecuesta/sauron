package status

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sauron/internal/testutil"
	"sauron/selector"
	"sauron/storage"

	"go.uber.org/zap"
)

// handlerYAML returns a V2 config YAML for handler tests.
// hasInternals controls whether internal nodes or external rings are included.
func handlerYAML(hasInternals bool) string {
	base := `
listen: ":3000"
auth: false
timeouts:
  health_check: 5s
  proxy: 30s
networks:
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: rest
        listen: ":8080"
        advertise: "https://api.pocket.example.com"
`
	if hasInternals {
		return base + `
internals:
  - name: node-1
    network: pocket
    endpoints:
      rest: "https://node1.example.com"
`
	}
	return base + `
externals:
  - name: ext-ring
    rings:
      - "https://ring1.example.com"
`
}

// buildHandler creates a Handler+HeightStore for handler tests.
func buildHandler(t *testing.T, hasInternals bool) (*Handler, *storage.HeightStore) {
	t.Helper()
	logger := zap.NewNop()
	loader := testutil.NewMultiChainLoader(t, handlerYAML(hasInternals))
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

	// Populate the height store so the selector has data.
	// For cosmos networks, the height-check protocol defaults to "rpc".
	heightStore.Update("pocket", "node-1", "rpc", 12345, 10*time.Millisecond, "internal")

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
