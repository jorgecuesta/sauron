package status

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

func TestAuth_UserInContext(t *testing.T) {
	t.Parallel()
	loader := createAuthTestLoader(t)
	h := buildHandlerForLoader(loader)

	var capturedUser *config.MultiChainUser
	capturing := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, ok := r.Context().Value(contextKeyUser).(*config.MultiChainUser); ok {
			capturedUser = user
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
	if capturedUser == nil {
		t.Fatal("expected user in context, got nil")
	}
	if capturedUser.Name != "alice" {
		t.Errorf("expected user 'alice', got %q", capturedUser.Name)
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

// createPerNetworkAuthLoader creates a config with two networks and users
// with per-network per-protocol permissions for testing granular access.
func createPerNetworkAuthLoader(t *testing.T) *config.MultiChainLoader {
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
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rest
        listen: ":8080"
        advertise: "https://pocket-rest.example.com"
      - protocol: rpc
        listen: ":8081"
        advertise: "https://pocket-rpc.example.com"
  - name: ethereum
    type: evm
    height_check:
      interval: 12s
    endpoints:
      - protocol: jsonrpc
        listen: ":9080"
        advertise: "https://eth-jsonrpc.example.com"
internals:
  - name: node-1
    network: pocket
    endpoints:
      rest: "http://node1:1317"
      rpc: "http://node1:26657"
  - name: eth-1
    network: ethereum
    endpoints:
      jsonrpc: "http://geth1:8545"
users:
  - name: admin
    token: "admin-token"
    permissions: all
  - name: pocket-rest-only
    token: "rest-token"
    permissions:
      pocket:
        rest: true
        rpc: false
  - name: eth-user
    token: "eth-token"
    permissions:
      ethereum:
        jsonrpc: true
`)
}

func TestAuth_PerNetworkPermissions_RestOnly(t *testing.T) {
	t.Parallel()

	loader := createPerNetworkAuthLoader(t)
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	sel := selector.NewSelector(heightStore, endpointStore, loader, logger)
	h := NewHandler(sel, loader, logger)
	t.Cleanup(h.Shutdown)

	// Seed heights so the status endpoint has data.
	heightStore.Update("pocket", "node-1", "rpc", 100, 5*time.Millisecond, "internal")

	mux := http.NewServeMux()
	h.SetupRoutes(mux)

	// User "pocket-rest-only" requests pocket/status — should only see "rest" endpoint.
	req := httptest.NewRequest(http.MethodGet, "/pocket/status", nil)
	req.Header.Set("Authorization", "Bearer rest-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	var resp StatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Endpoints["rest"] == "" {
		t.Error("expected rest endpoint in response, got empty")
	}
	if resp.Endpoints["rpc"] != "" {
		t.Errorf("expected no rpc endpoint for rest-only user, got %q", resp.Endpoints["rpc"])
	}
}

func TestAuth_PerNetworkPermissions_NoAccessToNetwork(t *testing.T) {
	t.Parallel()

	loader := createPerNetworkAuthLoader(t)
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	sel := selector.NewSelector(heightStore, endpointStore, loader, logger)
	h := NewHandler(sel, loader, logger)
	t.Cleanup(h.Shutdown)

	heightStore.Update("ethereum", "eth-1", "jsonrpc", 5000, 3*time.Millisecond, "internal")

	mux := http.NewServeMux()
	h.SetupRoutes(mux)

	// User "pocket-rest-only" requests ethereum/status — no permissions for ethereum.
	req := httptest.NewRequest(http.MethodGet, "/ethereum/status", nil)
	req.Header.Set("Authorization", "Bearer rest-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Should get 404 — user has no access to any ethereum protocols,
	// so enabledTypes is empty and heights map is empty.
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for user with no ethereum access, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestAuth_PerNetworkPermissions_AdminSeesAll(t *testing.T) {
	t.Parallel()

	loader := createPerNetworkAuthLoader(t)
	logger := zap.NewNop()
	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(logger)
	sel := selector.NewSelector(heightStore, endpointStore, loader, logger)
	h := NewHandler(sel, loader, logger)
	t.Cleanup(h.Shutdown)

	heightStore.Update("pocket", "node-1", "rpc", 100, 5*time.Millisecond, "internal")

	mux := http.NewServeMux()
	h.SetupRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/pocket/status", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp StatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Admin should see both rest and rpc endpoints.
	if resp.Endpoints["rest"] == "" {
		t.Error("expected rest endpoint for admin")
	}
	if resp.Endpoints["rpc"] == "" {
		t.Error("expected rpc endpoint for admin")
	}
}
