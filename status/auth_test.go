package status

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"sauron/config"
	"sauron/internal/testutil"
	"sauron/selector"
	"sauron/storage"

	"go.uber.org/zap"
)

// createAuthTestLoader writes a temporary YAML config with auth=true and one user,
// then returns a *config.MultiChainLoader backed by that file.
func createAuthTestLoader(t *testing.T) *config.MultiChainLoader {
	t.Helper()

	return testutil.NewMultiChainLoader(t, `
listen: ":3000"
auth: true
timeouts:
  health_check: 5s
  proxy: 30s
networks:
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: rest
        listen: ":8080"
internals:
  - name: node-1
    network: pocket
    endpoints:
      rest: "https://node1.example.com"
users:
  - name: alice
    token: "valid-token-abc"
    permissions: all
`)
}

// buildHandlerForLoader wires up a real Handler using the provided loader.
func buildHandlerForLoader(loader *config.MultiChainLoader) *Handler {
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	sel := selector.NewSelector(heightStore, endpointStore, loader, logger)
	return NewHandler(sel, loader, logger)
}

// noopHandler is a simple handler that records whether it was called.
func noopHandler(called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		w.WriteHeader(http.StatusOK)
	})
}

func TestAuth_ValidToken(t *testing.T) {
	t.Parallel()
	loader := createAuthTestLoader(t)
	h := buildHandlerForLoader(loader)

	called := false
	handler := h.authMiddleware(noopHandler(&called))

	req := httptest.NewRequest(http.MethodGet, "/pocket/status", nil)
	req.Header.Set("Authorization", "Bearer valid-token-abc")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !called {
		t.Error("expected next handler to be called")
	}
}

func TestAuth_MissingHeader(t *testing.T) {
	t.Parallel()
	loader := createAuthTestLoader(t)
	h := buildHandlerForLoader(loader)

	called := false
	handler := h.authMiddleware(noopHandler(&called))

	req := httptest.NewRequest(http.MethodGet, "/pocket/status", nil)
	// No Authorization header
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
	if called {
		t.Error("expected next handler NOT to be called")
	}
}

func TestAuth_InvalidFormat(t *testing.T) {
	t.Parallel()
	loader := createAuthTestLoader(t)
	h := buildHandlerForLoader(loader)

	called := false
	handler := h.authMiddleware(noopHandler(&called))

	req := httptest.NewRequest(http.MethodGet, "/pocket/status", nil)
	req.Header.Set("Authorization", "Basic valid-token-abc") // "Basic" not "Bearer"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
	if called {
		t.Error("expected next handler NOT to be called")
	}
}

func TestAuth_WrongToken(t *testing.T) {
	t.Parallel()
	loader := createAuthTestLoader(t)
	h := buildHandlerForLoader(loader)

	called := false
	handler := h.authMiddleware(noopHandler(&called))

	req := httptest.NewRequest(http.MethodGet, "/pocket/status", nil)
	req.Header.Set("Authorization", "Bearer wrong-token-xyz")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
	if called {
		t.Error("expected next handler NOT to be called")
	}
}

func TestAuth_EnabledTypesInContext(t *testing.T) {
	t.Parallel()
	loader := createAuthTestLoader(t)
	h := buildHandlerForLoader(loader)

	var capturedTypes []string
	capturing := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if types, ok := r.Context().Value(contextKeyEnabledTypes).([]string); ok {
			capturedTypes = types
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := h.authMiddleware(capturing)
	req := httptest.NewRequest(http.MethodGet, "/pocket/status", nil)
	req.Header.Set("Authorization", "Bearer valid-token-abc")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if len(capturedTypes) == 0 {
		t.Fatal("expected enabled types in context, got none")
	}
	found := false
	for _, typ := range capturedTypes {
		if typ == "rest" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'rest' in enabled types, got %v", capturedTypes)
	}
}

func TestAuth_EmptyToken(t *testing.T) {
	t.Parallel()
	loader := createAuthTestLoader(t)
	h := buildHandlerForLoader(loader)

	called := false
	handler := h.authMiddleware(noopHandler(&called))

	req := httptest.NewRequest(http.MethodGet, "/pocket/status", nil)
	req.Header.Set("Authorization", "Bearer ") // empty token after Bearer
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for empty token, got %d", w.Code)
	}
	if called {
		t.Error("expected next handler NOT to be called for empty token")
	}
}
