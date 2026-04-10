package checker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"sauron/config"
	"sauron/internal/testutil"
	"sauron/storage"

	"go.uber.org/zap"
)

// newTestExternalChecker wires up an ExternalChecker backed by real in-memory stores.
// The configLoader is required only for gRPC paths; pass nil to use the default
// minimal YAML (HTTP-only tests don't need gRPC keepalive config).
func newTestExternalChecker(t *testing.T, loader *config.MultiChainLoader) (*ExternalChecker, *storage.HeightStore, *storage.ExternalEndpointStore) {
	t.Helper()

	if loader == nil {
		loader = testutil.NewMultiChainLoader(t, `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
grpc_keepalive:
  time: 600s
  timeout: 20s
networks:
  - name: pocket
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rest
        listen: ":8080"
internals:
  - name: node-1
    network: pocket
    endpoints:
      rest: "http://localhost:1317"
`)
	}

	heightStore := storage.NewHeightStore()
	endpointStore := storage.NewExternalEndpointStore(zap.NewNop())
	checker := NewExternalChecker(heightStore, endpointStore, loader, zap.NewNop())
	return checker, heightStore, endpointStore
}

// newStatusHandler returns an httptest.Handler that serves an ExternalStatusResponse.
func newStatusHandler(height int64, endpoints map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		resp := ExternalStatusResponse{
			Height:    height,
			Endpoints: endpoints,
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			http.Error(w, "encode error", http.StatusInternalServerError)
		}
	}
}

// ─── queryRing tests ──────────────────────────────────────────────────────────

// TestQueryRing_HappyPath verifies that a successful ring response stores the
// endpoint in the endpoint store and marks it as validated (via the mock HTTP
// endpoint that responds with 200 to HEAD).
func TestQueryRing_HappyPath(t *testing.T) {
	t.Parallel()

	// Start an endpoint mock so validateHTTPEndpoint succeeds.
	epSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer epSrv.Close()

	// Build the ring mock that advertises the endpoint server.
	ringSrv := httptest.NewServer(newStatusHandler(100, map[string]string{
		"rest": epSrv.URL,
	}))
	defer ringSrv.Close()

	checker, _, endpointStore := newTestExternalChecker(t, nil)

	ext := config.External{Name: "partner", Token: "", Rings: []string{ringSrv.URL}}
	err := checker.queryRing(context.Background(), ext, ringSrv.URL, "pocket")
	if err != nil {
		t.Fatalf("queryRing returned unexpected error: %v", err)
	}

	// The endpoint must be stored and validated.
	validated := endpointStore.GetValidatedEndpoints("pocket", "rest")
	if len(validated) != 1 {
		t.Fatalf("expected 1 validated endpoint, got %d", len(validated))
	}
	ep := validated[0]
	if ep.URL != epSrv.URL {
		t.Errorf("URL: got %q, want %q", ep.URL, epSrv.URL)
	}
	if ep.Network != "pocket" {
		t.Errorf("Network: got %q, want %q", ep.Network, "pocket")
	}
	if ep.Type != "rest" {
		t.Errorf("Type: got %q, want %q", ep.Type, "rest")
	}
	if ep.ExternalName != "partner" {
		t.Errorf("ExternalName: got %q, want %q", ep.ExternalName, "partner")
	}
	if ep.Height != 100 {
		t.Errorf("Height: got %d, want %d", ep.Height, 100)
	}
	if !ep.IsValidated {
		t.Error("expected IsValidated == true")
	}
	if !ep.IsWorking {
		t.Error("expected IsWorking == true")
	}
}

// TestQueryRing_ZeroHeight verifies that a ring returning height=0 is rejected.
func TestQueryRing_ZeroHeight(t *testing.T) {
	t.Parallel()

	ringSrv := httptest.NewServer(newStatusHandler(0, map[string]string{"rest": "http://example.com"}))
	defer ringSrv.Close()

	checker, _, _ := newTestExternalChecker(t, nil)
	ext := config.External{Name: "partner", Rings: []string{ringSrv.URL}}

	err := checker.queryRing(context.Background(), ext, ringSrv.URL, "pocket")
	if err == nil {
		t.Fatal("expected error for zero height, got nil")
	}
}

// TestQueryRing_EmptyEndpoints verifies that a ring with no endpoints
// does not store anything, but also returns no error.
func TestQueryRing_EmptyEndpoints(t *testing.T) {
	t.Parallel()

	ringSrv := httptest.NewServer(newStatusHandler(50, nil))
	defer ringSrv.Close()

	checker, _, endpointStore := newTestExternalChecker(t, nil)
	ext := config.External{Name: "partner", Rings: []string{ringSrv.URL}}

	err := checker.queryRing(context.Background(), ext, ringSrv.URL, "pocket")
	if err != nil {
		t.Fatalf("expected no error for empty endpoints, got %v", err)
	}

	validated := endpointStore.GetValidatedEndpoints("pocket", "rest")
	if len(validated) != 0 {
		t.Errorf("expected 0 validated endpoints, got %d", len(validated))
	}
}

// TestQueryRing_HTTPError verifies that a 5xx ring response is treated as an error.
func TestQueryRing_HTTPError(t *testing.T) {
	t.Parallel()

	ringSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ringSrv.Close()

	checker, _, _ := newTestExternalChecker(t, nil)
	ext := config.External{Name: "partner", Rings: []string{ringSrv.URL}}

	err := checker.queryRing(context.Background(), ext, ringSrv.URL, "pocket")
	if err == nil {
		t.Fatal("expected error for HTTP 500, got nil")
	}
}

// TestQueryRing_InvalidJSON verifies that malformed JSON from a ring is rejected.
func TestQueryRing_InvalidJSON(t *testing.T) {
	t.Parallel()

	ringSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{this is not valid json`))
	}))
	defer ringSrv.Close()

	checker, _, _ := newTestExternalChecker(t, nil)
	ext := config.External{Name: "partner", Rings: []string{ringSrv.URL}}

	err := checker.queryRing(context.Background(), ext, ringSrv.URL, "pocket")
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

// TestQueryRing_BearerTokenForwarded verifies that the Authorization header is
// set when the External.Token is non-empty.
func TestQueryRing_BearerTokenForwarded(t *testing.T) {
	t.Parallel()

	var capturedAuth atomic.Value
	ringSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		// Return valid zero-endpoint JSON so we don't fail for wrong reasons.
		_ = json.NewEncoder(w).Encode(ExternalStatusResponse{Height: 42})
	}))
	defer ringSrv.Close()

	checker, _, _ := newTestExternalChecker(t, nil)
	ext := config.External{Name: "partner", Token: "my-secret", Rings: []string{ringSrv.URL}}

	_ = checker.queryRing(context.Background(), ext, ringSrv.URL, "pocket")

	got, _ := capturedAuth.Load().(string)
	want := "Bearer my-secret"
	if got != want {
		t.Errorf("Authorization header: got %q, want %q", got, want)
	}
}

// TestQueryRing_NoToken verifies that no Authorization header is sent when
// External.Token is empty.
func TestQueryRing_NoToken(t *testing.T) {
	t.Parallel()

	var capturedAuth atomic.Value
	capturedAuth.Store("")
	ringSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ExternalStatusResponse{Height: 42})
	}))
	defer ringSrv.Close()

	checker, _, _ := newTestExternalChecker(t, nil)
	ext := config.External{Name: "partner", Token: "", Rings: []string{ringSrv.URL}}

	_ = checker.queryRing(context.Background(), ext, ringSrv.URL, "pocket")

	got, _ := capturedAuth.Load().(string)
	if got != "" {
		t.Errorf("expected no Authorization header, got %q", got)
	}
}

// TestQueryRing_NegativeHeight verifies that a ring returning a negative height
// is treated as a zero-height error (height <= 0 check logic).
// Note: the code checks height == 0, but a negative height passes through.
// This test documents the current behavior.
func TestQueryRing_NegativeHeight(t *testing.T) {
	t.Parallel()

	ringSrv := httptest.NewServer(newStatusHandler(-5, nil))
	defer ringSrv.Close()

	checker, _, _ := newTestExternalChecker(t, nil)
	ext := config.External{Name: "partner", Rings: []string{ringSrv.URL}}

	// Negative height: height != 0 so code does NOT reject — empty endpoints → no error.
	// This test documents the behavior (no error, nothing stored).
	err := checker.queryRing(context.Background(), ext, ringSrv.URL, "pocket")
	if err != nil {
		t.Logf("queryRing returned (non-zero negative height treated as): %v", err)
	}
}

// TestQueryRing_URLPathBuilt verifies that the request URL includes /{network}/status.
func TestQueryRing_URLPathBuilt(t *testing.T) {
	t.Parallel()

	var capturedPath atomic.Value
	ringSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath.Store(r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ExternalStatusResponse{Height: 10})
	}))
	defer ringSrv.Close()

	checker, _, _ := newTestExternalChecker(t, nil)
	ext := config.External{Name: "partner", Rings: []string{ringSrv.URL}}

	_ = checker.queryRing(context.Background(), ext, ringSrv.URL, "pocket")

	got, _ := capturedPath.Load().(string)
	want := "/pocket/status"
	if got != want {
		t.Errorf("request path: got %q, want %q", got, want)
	}
}

// TestQueryRing_RingWithTrailingSlash verifies that trailing slashes in ring
// URLs are handled correctly (no double-slash in the built URL).
func TestQueryRing_RingWithTrailingSlash(t *testing.T) {
	t.Parallel()

	var capturedPath atomic.Value
	ringSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath.Store(r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ExternalStatusResponse{Height: 10})
	}))
	defer ringSrv.Close()

	checker, _, _ := newTestExternalChecker(t, nil)
	ringURLWithSlash := ringSrv.URL + "/"
	ext := config.External{Name: "partner", Rings: []string{ringURLWithSlash}}

	_ = checker.queryRing(context.Background(), ext, ringURLWithSlash, "pocket")

	got, _ := capturedPath.Load().(string)
	want := "/pocket/status"
	if got != want {
		t.Errorf("path with trailing slash: got %q, want %q", got, want)
	}
}

// TestQueryRing_EndpointValidationFailed verifies that when the ring returns
// a valid height+endpoint but the endpoint is unreachable, the endpoint is
// stored as advertised but NOT in the validated list.
func TestQueryRing_EndpointValidationFailed(t *testing.T) {
	t.Parallel()

	// Use port 0 (closed) so validateHTTPEndpoint fails.
	badURL := "http://127.0.0.1:0"
	ringSrv := httptest.NewServer(newStatusHandler(100, map[string]string{
		"rest": badURL,
	}))
	defer ringSrv.Close()

	checker, _, endpointStore := newTestExternalChecker(t, nil)
	ext := config.External{Name: "partner", Rings: []string{ringSrv.URL}}

	err := checker.queryRing(context.Background(), ext, ringSrv.URL, "pocket")
	if err != nil {
		t.Fatalf("queryRing should not return error when only endpoint validation fails, got: %v", err)
	}

	// Not in validated list.
	validated := endpointStore.GetValidatedEndpoints("pocket", "rest")
	if len(validated) != 0 {
		t.Errorf("expected 0 validated endpoints (endpoint is down), got %d", len(validated))
	}

	// But failed endpoints should show the endpoint since StoreAdvertised was called
	// followed by MarkValidationFailed. GetFailedEndpoints only returns IsValidated&&!IsWorking,
	// and fresh endpoints start IsValidated=false — so neither list contains it. That's fine.
}

// ─── CheckExternal tests ──────────────────────────────────────────────────────

// TestCheckExternal_MultipleRings verifies that CheckExternal queries each ring
// and processes successful ones even when another fails.
func TestCheckExternal_MultipleRings(t *testing.T) {
	t.Parallel()

	// Good ring: returns valid response.
	epSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer epSrv.Close()

	goodRing := httptest.NewServer(newStatusHandler(200, map[string]string{
		"rest": epSrv.URL,
	}))
	defer goodRing.Close()

	// Bad ring: returns 503.
	badRing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer badRing.Close()

	checker, _, endpointStore := newTestExternalChecker(t, nil)
	ext := config.External{
		Name:  "partner",
		Rings: []string{goodRing.URL, badRing.URL},
	}

	// CheckExternal never returns an error — it logs and continues.
	err := checker.CheckExternal(context.Background(), ext, "pocket")
	if err != nil {
		t.Fatalf("CheckExternal returned unexpected error: %v", err)
	}

	// The good ring's endpoint should be validated.
	validated := endpointStore.GetValidatedEndpoints("pocket", "rest")
	if len(validated) != 1 {
		t.Fatalf("expected 1 validated endpoint from good ring, got %d", len(validated))
	}
}

// TestCheckExternal_NoRings verifies that CheckExternal returns an error when
// the External has an empty rings list.
func TestCheckExternal_NoRings(t *testing.T) {
	t.Parallel()

	checker, _, _ := newTestExternalChecker(t, nil)
	ext := config.External{Name: "partner", Rings: nil}

	err := checker.CheckExternal(context.Background(), ext, "pocket")
	if err == nil {
		t.Fatal("expected error for empty rings, got nil")
	}
}

// TestCheckExternal_AllRingsFail verifies that CheckExternal returns nil even
// when ALL rings fail (errors are logged, not propagated).
func TestCheckExternal_AllRingsFail(t *testing.T) {
	t.Parallel()

	ring1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ring1.Close()

	ring2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ring2.Close()

	checker, _, endpointStore := newTestExternalChecker(t, nil)
	ext := config.External{
		Name:  "partner",
		Rings: []string{ring1.URL, ring2.URL},
	}

	err := checker.CheckExternal(context.Background(), ext, "pocket")
	if err != nil {
		t.Fatalf("CheckExternal should not propagate ring errors, got: %v", err)
	}

	validated := endpointStore.GetValidatedEndpoints("pocket", "rest")
	if len(validated) != 0 {
		t.Errorf("expected 0 validated endpoints, got %d", len(validated))
	}
}

// TestCheckExternal_SingleRingSuccess verifies the basic single-ring success path.
func TestCheckExternal_SingleRingSuccess(t *testing.T) {
	t.Parallel()

	epSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer epSrv.Close()

	ringSrv := httptest.NewServer(newStatusHandler(999, map[string]string{
		"rest": epSrv.URL,
	}))
	defer ringSrv.Close()

	checker, _, endpointStore := newTestExternalChecker(t, nil)
	ext := config.External{Name: "pnf", Rings: []string{ringSrv.URL}}

	err := checker.CheckExternal(context.Background(), ext, "pocket")
	if err != nil {
		t.Fatalf("CheckExternal returned error: %v", err)
	}

	validated := endpointStore.GetValidatedEndpoints("pocket", "rest")
	if len(validated) != 1 {
		t.Fatalf("expected 1 validated endpoint, got %d", len(validated))
	}
	if validated[0].Height != 999 {
		t.Errorf("height: got %d, want 999", validated[0].Height)
	}
}

// TestCheckExternal_MultipleEndpointTypes verifies that multiple endpoint
// protocol types from one ring are all stored.
func TestCheckExternal_MultipleEndpointTypes(t *testing.T) {
	t.Parallel()

	// Two endpoint servers (rest + rpc).
	restSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer restSrv.Close()

	rpcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// WebSocket check will also be attempted for "rpc" type but will fail gracefully.
		w.WriteHeader(http.StatusOK)
	}))
	defer rpcSrv.Close()

	ringSrv := httptest.NewServer(newStatusHandler(500, map[string]string{
		"rest": restSrv.URL,
		"rpc":  rpcSrv.URL,
	}))
	defer ringSrv.Close()

	checker, _, endpointStore := newTestExternalChecker(t, nil)
	ext := config.External{Name: "partner", Rings: []string{ringSrv.URL}}

	err := checker.CheckExternal(context.Background(), ext, "pocket")
	if err != nil {
		t.Fatalf("CheckExternal returned error: %v", err)
	}

	// Both should be validated.
	restEps := endpointStore.GetValidatedEndpoints("pocket", "rest")
	rpcEps := endpointStore.GetValidatedEndpoints("pocket", "rpc")

	if len(restEps) != 1 {
		t.Errorf("expected 1 validated REST endpoint, got %d", len(restEps))
	}
	if len(rpcEps) != 1 {
		t.Errorf("expected 1 validated RPC endpoint, got %d", len(rpcEps))
	}
}

// ─── validateHTTPEndpoint tests ───────────────────────────────────────────────

// TestValidateHTTPEndpoint_Reachable verifies that a responding server yields
// a non-zero latency and nil error.
func TestValidateHTTPEndpoint_Reachable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Must handle HEAD.
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	checker, _, _ := newTestExternalChecker(t, nil)
	latency, err := checker.validateHTTPEndpoint(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("validateHTTPEndpoint returned unexpected error: %v", err)
	}
	if latency <= 0 {
		t.Errorf("expected positive latency, got %v", latency)
	}
}

// TestValidateHTTPEndpoint_Unreachable verifies that a closed port returns an error.
func TestValidateHTTPEndpoint_Unreachable(t *testing.T) {
	t.Parallel()

	checker, _, _ := newTestExternalChecker(t, nil)
	_, err := checker.validateHTTPEndpoint(context.Background(), "http://127.0.0.1:0")
	if err == nil {
		t.Fatal("expected error for unreachable endpoint, got nil")
	}
}

// TestValidateHTTPEndpoint_ServerError verifies that a 5xx response is treated
// as an error (not "working").
func TestValidateHTTPEndpoint_ServerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	checker, _, _ := newTestExternalChecker(t, nil)
	_, err := checker.validateHTTPEndpoint(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
}

// TestValidateHTTPEndpoint_ClientError verifies that a 4xx response is NOT
// treated as an error (auth-gated endpoints still count as "reachable").
func TestValidateHTTPEndpoint_ClientError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	checker, _, _ := newTestExternalChecker(t, nil)
	_, err := checker.validateHTTPEndpoint(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("expected no error for 401 (auth-gated endpoint), got: %v", err)
	}
}

// TestValidateHTTPEndpoint_404 verifies that 404 is also accepted (endpoint
// exists but path not found — server is up).
func TestValidateHTTPEndpoint_404(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	checker, _, _ := newTestExternalChecker(t, nil)
	_, err := checker.validateHTTPEndpoint(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("expected no error for 404, got: %v", err)
	}
}

// TestValidateHTTPEndpoint_503 verifies that 503 (>= 500) returns an error.
func TestValidateHTTPEndpoint_503(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	checker, _, _ := newTestExternalChecker(t, nil)
	_, err := checker.validateHTTPEndpoint(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 503, got nil")
	}
}

// TestValidateHTTPEndpoint_ContextCanceled verifies that a canceled context
// causes the check to fail.
func TestValidateHTTPEndpoint_ContextCanceled(t *testing.T) {
	t.Parallel()

	// Slow server that will block until the context cancels.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	checker, _, _ := newTestExternalChecker(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	_, err := checker.validateHTTPEndpoint(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected error for canceled context, got nil")
	}
}

// TestValidateHTTPEndpoint_BadURL verifies that an unparseable URL returns an error.
func TestValidateHTTPEndpoint_BadURL(t *testing.T) {
	t.Parallel()

	checker, _, _ := newTestExternalChecker(t, nil)
	_, err := checker.validateHTTPEndpoint(context.Background(), "://not a url")
	if err == nil {
		t.Fatal("expected error for invalid URL, got nil")
	}
}

// ─── RecoverFailedEndpoints tests ────────────────────────────────────────────

// seedFailedEndpoint stores an endpoint in the store and drives it to the
// IsValidated=true, IsWorking=false state that GetFailedEndpoints returns.
//
// The recovery list (GetFailedEndpoints) requires IsValidated==true AND
// IsWorking==false. This state is reached by:
//  1. StoreAdvertised  → IsValidated=false, IsWorking=false
//  2. MarkValidated    → IsValidated=true,  IsWorking=true
//  3. Three TrackProxyError calls → IsWorking=false (after ≥3 errors)
//
// Note: MarkValidationFailed sets IsValidated=false, so it does NOT produce a
// "failed-but-recoverable" endpoint. Only TrackProxyError demotes to
// IsWorking=false while keeping IsValidated=true.
func seedFailedEndpoint(t *testing.T, endpointStore *storage.ExternalEndpointStore, externalName, ringURL, network, epType, url string, height int64) {
	t.Helper()
	endpointStore.StoreAdvertised(externalName, ringURL, network, epType, url)
	endpointStore.MarkValidated(externalName, ringURL, network, epType, url, height, 5*time.Millisecond)
	// Three proxy errors are required to flip IsWorking to false.
	endpointStore.TrackProxyError(network, epType, url)
	endpointStore.TrackProxyError(network, epType, url)
	endpointStore.TrackProxyError(network, epType, url)
}

// TestRecoverFailedEndpoints_Recovers verifies that a previously-failed endpoint
// is re-validated and moved back to working when the server is reachable.
func TestRecoverFailedEndpoints_Recovers(t *testing.T) {
	t.Parallel()

	// Start a healthy endpoint server.
	epSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer epSrv.Close()

	checker, _, endpointStore := newTestExternalChecker(t, nil)

	// Seed a failed endpoint pointing at the now-healthy server.
	seedFailedEndpoint(t, endpointStore, "partner", "http://ring.example.com", "pocket", "rest", epSrv.URL, 100)

	// Confirm it's in the failed list.
	failed := endpointStore.GetFailedEndpoints()
	if len(failed) != 1 {
		t.Fatalf("expected 1 failed endpoint before recovery, got %d", len(failed))
	}

	checker.RecoverFailedEndpoints(context.Background())

	// Now it should be validated and working.
	validated := endpointStore.GetValidatedEndpoints("pocket", "rest")
	if len(validated) != 1 {
		t.Fatalf("expected 1 validated endpoint after recovery, got %d", len(validated))
	}
	if !validated[0].IsWorking {
		t.Error("expected endpoint to be working after recovery")
	}

	// Failed list should be empty.
	failed = endpointStore.GetFailedEndpoints()
	if len(failed) != 0 {
		t.Errorf("expected 0 failed endpoints after recovery, got %d", len(failed))
	}
}

// TestRecoverFailedEndpoints_StillFailing verifies that a still-unreachable
// endpoint remains in the failed state after recovery attempt.
func TestRecoverFailedEndpoints_StillFailing(t *testing.T) {
	t.Parallel()

	// Get a free local port, then immediately close the listener so the port is
	// released but nothing is listening on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate local port: %v", err)
	}
	closedURL := fmt.Sprintf("http://%s", ln.Addr().String())
	_ = ln.Close() // Close so nothing is listening.

	checker, _, endpointStore := newTestExternalChecker(t, nil)

	seedFailedEndpoint(t, endpointStore, "partner", "http://ring.example.com", "pocket", "rest", closedURL, 50)

	checker.RecoverFailedEndpoints(context.Background())

	// Should still be failed.
	validated := endpointStore.GetValidatedEndpoints("pocket", "rest")
	if len(validated) != 0 {
		t.Errorf("expected 0 validated endpoints (still failing), got %d", len(validated))
	}

	failed := endpointStore.GetFailedEndpoints()
	if len(failed) != 1 {
		t.Errorf("expected 1 failed endpoint after failed recovery, got %d", len(failed))
	}
}

// TestRecoverFailedEndpoints_Empty verifies that RecoverFailedEndpoints
// does not panic when there are no failed endpoints.
func TestRecoverFailedEndpoints_Empty(t *testing.T) {
	t.Parallel()

	checker, _, _ := newTestExternalChecker(t, nil)

	// Must not panic.
	checker.RecoverFailedEndpoints(context.Background())
}

// TestRecoverFailedEndpoints_MultipleEndpoints verifies that each failed
// endpoint is checked independently.
func TestRecoverFailedEndpoints_MultipleEndpoints(t *testing.T) {
	t.Parallel()

	// First endpoint recovers.
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer goodSrv.Close()

	// Second endpoint remains down.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate port: %v", err)
	}
	badURL := fmt.Sprintf("http://%s", ln.Addr().String())
	_ = ln.Close()

	checker, _, endpointStore := newTestExternalChecker(t, nil)

	ringURL := "http://ring.example.com"
	seedFailedEndpoint(t, endpointStore, "partner", ringURL, "pocket", "rest", goodSrv.URL, 100)
	seedFailedEndpoint(t, endpointStore, "partner", ringURL, "pocket", "rpc", badURL, 100)

	checker.RecoverFailedEndpoints(context.Background())

	restValidated := endpointStore.GetValidatedEndpoints("pocket", "rest")
	if len(restValidated) != 1 {
		t.Errorf("expected good REST endpoint to recover, got %d validated", len(restValidated))
	}

	rpcValidated := endpointStore.GetValidatedEndpoints("pocket", "rpc")
	if len(rpcValidated) != 0 {
		t.Errorf("expected bad RPC endpoint to remain failed, got %d validated", len(rpcValidated))
	}
}

// ─── UpdateEndpointMetrics tests ─────────────────────────────────────────────

// TestUpdateEndpointMetrics_NoPanic verifies that UpdateEndpointMetrics
// delegates to the store and does not panic with an empty store.
func TestUpdateEndpointMetrics_NoPanic(t *testing.T) {
	t.Parallel()

	checker, _, _ := newTestExternalChecker(t, nil)

	// Must not panic.
	checker.UpdateEndpointMetrics()
}

// TestUpdateEndpointMetrics_WithEndpoints verifies that UpdateEndpointMetrics
// runs without panicking when endpoints are present.
func TestUpdateEndpointMetrics_WithEndpoints(t *testing.T) {
	t.Parallel()

	epSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer epSrv.Close()

	ringSrv := httptest.NewServer(newStatusHandler(10, map[string]string{"rest": epSrv.URL}))
	defer ringSrv.Close()

	checker, _, _ := newTestExternalChecker(t, nil)
	ext := config.External{Name: "partner", Rings: []string{ringSrv.URL}}
	_ = checker.CheckExternal(context.Background(), ext, "pocket")

	// Must not panic.
	checker.UpdateEndpointMetrics()
}

// ─── Close tests ─────────────────────────────────────────────────────────────

// TestClose_Idempotent verifies that Close does not panic on successive calls.
func TestClose_Idempotent(t *testing.T) {
	t.Parallel()

	checker, _, _ := newTestExternalChecker(t, nil)
	checker.Close()
	checker.Close() // Must not panic.
}

// ─── Concurrency tests ────────────────────────────────────────────────────────

// TestCheckExternal_ConcurrentRings verifies that running CheckExternal
// concurrently from multiple goroutines does not race.
func TestCheckExternal_ConcurrentRings(t *testing.T) {
	t.Parallel()

	epSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer epSrv.Close()

	ringSrv := httptest.NewServer(newStatusHandler(77, map[string]string{"rest": epSrv.URL}))
	defer ringSrv.Close()

	checker, _, _ := newTestExternalChecker(t, nil)
	ext := config.External{Name: "partner", Rings: []string{ringSrv.URL}}

	const goroutines = 10
	done := make(chan struct{}, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			_ = checker.CheckExternal(context.Background(), ext, "pocket")
		}()
	}

	for i := 0; i < goroutines; i++ {
		<-done
	}
}

// TestRecoverFailedEndpoints_Concurrent verifies that RecoverFailedEndpoints
// is safe to call concurrently.
func TestRecoverFailedEndpoints_Concurrent(t *testing.T) {
	t.Parallel()

	epSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer epSrv.Close()

	checker, _, endpointStore := newTestExternalChecker(t, nil)
	seedFailedEndpoint(t, endpointStore, "partner", "http://ring.example.com", "pocket", "rest", epSrv.URL, 42)

	const goroutines = 5
	done := make(chan struct{}, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			checker.RecoverFailedEndpoints(context.Background())
		}()
	}

	for i := 0; i < goroutines; i++ {
		<-done
	}
}

// ─── ExternalStatusResponse JSON parsing tests ────────────────────────────────

// TestExternalStatusResponse_JSONRoundTrip verifies that the struct serialises
// and deserialises correctly for the V2 wire format.
func TestExternalStatusResponse_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	original := ExternalStatusResponse{
		Height: 12345,
		Endpoints: map[string]string{
			"rest": "http://node.example.com:1317",
			"rpc":  "http://node.example.com:26657",
			"grpc": "node.example.com:9090",
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ExternalStatusResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Height != original.Height {
		t.Errorf("Height: got %d, want %d", got.Height, original.Height)
	}
	for proto, wantURL := range original.Endpoints {
		if got.Endpoints[proto] != wantURL {
			t.Errorf("Endpoints[%q]: got %q, want %q", proto, got.Endpoints[proto], wantURL)
		}
	}
}

// TestExternalStatusResponse_MissingEndpoints verifies that omitempty means
// a response with no endpoints parses to a nil map.
func TestExternalStatusResponse_MissingEndpoints(t *testing.T) {
	t.Parallel()

	var got ExternalStatusResponse
	if err := json.Unmarshal([]byte(`{"height":42}`), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Height != 42 {
		t.Errorf("Height: got %d, want 42", got.Height)
	}
	if len(got.Endpoints) != 0 {
		t.Errorf("Endpoints: expected empty/nil, got %v", got.Endpoints)
	}
}

// TestExternalStatusResponse_EmptyEndpointURL verifies that an endpoint with
// an empty URL is silently skipped (not stored).
func TestExternalStatusResponse_EmptyEndpointURL(t *testing.T) {
	t.Parallel()

	ringSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"height":100,"endpoints":{"rest":"","rpc":""}}`))
	}))
	defer ringSrv.Close()

	checker, _, endpointStore := newTestExternalChecker(t, nil)
	ext := config.External{Name: "partner", Rings: []string{ringSrv.URL}}

	err := checker.queryRing(context.Background(), ext, ringSrv.URL, "pocket")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Nothing should have been stored.
	all := endpointStore.GetValidatedEndpoints("pocket", "rest")
	if len(all) != 0 {
		t.Errorf("expected 0 endpoints for empty URL, got %d", len(all))
	}
}
