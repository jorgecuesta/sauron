package status

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

// TestNewRateLimiter_Creates verifies that NewRateLimiter returns a non-nil limiter
// with the configured parameters.
func TestNewRateLimiter_Creates(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(10, 20, false)
	if rl == nil {
		t.Fatal("expected non-nil RateLimiter")
	}
	defer rl.Stop()

	if rl.requestsPerIP != 10 {
		t.Errorf("expected requestsPerIP=10, got %d", rl.requestsPerIP)
	}
	if rl.burst != 20 {
		t.Errorf("expected burst=20, got %d", rl.burst)
	}
	if rl.trustProxy != false {
		t.Errorf("expected trustProxy=false, got %v", rl.trustProxy)
	}
	if rl.stopCh == nil {
		t.Error("expected non-nil stopCh")
	}
	if rl.limiters == nil {
		t.Error("expected non-nil limiters map")
	}
}

// TestNewRateLimiter_TrustProxy verifies that trustProxy=true is stored.
func TestNewRateLimiter_TrustProxy(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(5, 5, true)
	defer rl.Stop()

	if !rl.trustProxy {
		t.Error("expected trustProxy=true")
	}
}

// TestAllow_UnderLimit verifies that requests below the rate limit are all allowed.
func TestAllow_UnderLimit(t *testing.T) {
	t.Parallel()
	// 10 req/s with burst 10 — sending 5 should all be allowed
	rl := NewRateLimiter(10, 10, false)
	defer rl.Stop()

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.168.1.1:12345"
		if !rl.Allow(req) {
			t.Errorf("request %d expected to be allowed, was denied", i+1)
		}
	}
}

// TestAllow_ExceedsLimit verifies that once burst is exhausted subsequent requests are denied.
func TestAllow_ExceedsLimit(t *testing.T) {
	t.Parallel()
	// 1 req/s with burst 1 — first allowed, subsequent denied immediately
	rl := NewRateLimiter(1, 1, false)
	defer rl.Stop()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:9999"

	// First request uses the single token — allowed
	if !rl.Allow(req) {
		t.Fatal("first request should be allowed")
	}
	// Second and third requests exceed the burst — denied
	for i := 2; i <= 3; i++ {
		if rl.Allow(req) {
			t.Errorf("request %d should be denied (burst exhausted)", i)
		}
	}
}

// TestAllow_BurstCapacity verifies that exactly burst requests are allowed before denial.
func TestAllow_BurstCapacity(t *testing.T) {
	t.Parallel()
	// 1 req/s burst 5 — first 5 should be allowed, 6th denied
	rl := NewRateLimiter(1, 5, false)
	defer rl.Stop()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.16.0.1:4321"

	for i := 1; i <= 5; i++ {
		if !rl.Allow(req) {
			t.Errorf("request %d should be allowed (burst=%d)", i, 5)
		}
	}
	if rl.Allow(req) {
		t.Error("6th request should be denied after burst exhausted")
	}
}

// TestAllow_PerIPIsolation verifies that two different IPs have independent token buckets.
func TestAllow_PerIPIsolation(t *testing.T) {
	t.Parallel()
	// 1 req/s burst 1 — exhaust IP1, IP2 should still have tokens
	rl := NewRateLimiter(1, 1, false)
	defer rl.Stop()

	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	req1.RemoteAddr = "1.1.1.1:1111"

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.RemoteAddr = "2.2.2.2:2222"

	// Exhaust IP1's burst
	if !rl.Allow(req1) {
		t.Fatal("IP1 first request should be allowed")
	}
	if rl.Allow(req1) {
		t.Error("IP1 second request should be denied")
	}

	// IP2 has never been seen — its bucket is fresh
	if !rl.Allow(req2) {
		t.Error("IP2 should be allowed independently of IP1")
	}
}

// TestGetClientIP_WithoutTrustProxy verifies that RemoteAddr is used directly
// even if proxy headers are present.
func TestGetClientIP_WithoutTrustProxy(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(10, 10, false)
	defer rl.Stop()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.5.5:8080"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Real-IP", "5.6.7.8")

	got := rl.getClientIP(req)
	if got != "192.168.5.5" {
		t.Errorf("expected RemoteAddr IP 192.168.5.5, got %q", got)
	}
}

// TestGetClientIP_WithTrustProxy_XForwardedFor verifies that X-Forwarded-For
// is used as the client IP when trustProxy is true.
func TestGetClientIP_WithTrustProxy_XForwardedFor(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(10, 10, true)
	defer rl.Stop()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:9090"
	req.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.1")

	got := rl.getClientIP(req)
	if got != "203.0.113.5" {
		t.Errorf("expected X-Forwarded-For leftmost IP 203.0.113.5, got %q", got)
	}
}

// TestGetClientIP_WithTrustProxy_XForwardedFor_Single verifies single-IP XFF header.
func TestGetClientIP_WithTrustProxy_XForwardedFor_Single(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(10, 10, true)
	defer rl.Stop()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:9090"
	req.Header.Set("X-Forwarded-For", "198.51.100.42")

	got := rl.getClientIP(req)
	if got != "198.51.100.42" {
		t.Errorf("expected 198.51.100.42, got %q", got)
	}
}

// TestGetClientIP_WithTrustProxy_XRealIP verifies that X-Real-IP is used
// when X-Forwarded-For is absent.
func TestGetClientIP_WithTrustProxy_XRealIP(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(10, 10, true)
	defer rl.Stop()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:9090"
	req.Header.Set("X-Real-IP", "198.51.100.77")

	got := rl.getClientIP(req)
	if got != "198.51.100.77" {
		t.Errorf("expected X-Real-IP 198.51.100.77, got %q", got)
	}
}

// TestGetClientIP_WithTrustProxy_CFConnectingIP verifies that CF-Connecting-IP
// is used when XFF and X-Real-IP are absent.
func TestGetClientIP_WithTrustProxy_CFConnectingIP(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(10, 10, true)
	defer rl.Stop()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:9090"
	req.Header.Set("CF-Connecting-IP", "203.0.113.99")

	got := rl.getClientIP(req)
	if got != "203.0.113.99" {
		t.Errorf("expected CF-Connecting-IP 203.0.113.99, got %q", got)
	}
}

// TestGetClientIP_WithTrustProxy_TrueClientIP verifies that True-Client-IP
// is used when XFF, X-Real-IP, and CF-Connecting-IP are absent.
func TestGetClientIP_WithTrustProxy_TrueClientIP(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(10, 10, true)
	defer rl.Stop()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:9090"
	req.Header.Set("True-Client-IP", "203.0.113.200")

	got := rl.getClientIP(req)
	if got != "203.0.113.200" {
		t.Errorf("expected True-Client-IP 203.0.113.200, got %q", got)
	}
}

// TestGetClientIP_WithTrustProxy_FallbackToRemoteAddr verifies fallback to
// RemoteAddr when no valid proxy headers are present, even with trustProxy=true.
func TestGetClientIP_WithTrustProxy_FallbackToRemoteAddr(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(10, 10, true)
	defer rl.Stop()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:54321"
	// No proxy headers set

	got := rl.getClientIP(req)
	if got != "192.0.2.1" {
		t.Errorf("expected fallback to RemoteAddr 192.0.2.1, got %q", got)
	}
}

// TestGetClientIP_WithTrustProxy_InvalidXFF verifies that an invalid XFF IP
// falls through to the next header.
func TestGetClientIP_WithTrustProxy_InvalidXFF(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(10, 10, true)
	defer rl.Stop()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:9090"
	req.Header.Set("X-Forwarded-For", "not-a-valid-ip")
	req.Header.Set("X-Real-IP", "198.51.100.50")

	got := rl.getClientIP(req)
	// XFF is invalid, so should fall through to X-Real-IP
	if got != "198.51.100.50" {
		t.Errorf("expected fallback to X-Real-IP 198.51.100.50, got %q", got)
	}
}

// TestGetClientIP_XFFHeaderPriority verifies that XFF takes priority over X-Real-IP
// when trustProxy is true.
func TestGetClientIP_XFFHeaderPriority(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(10, 10, true)
	defer rl.Stop()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:9090"
	req.Header.Set("X-Forwarded-For", "203.0.113.10")
	req.Header.Set("X-Real-IP", "203.0.113.20")

	got := rl.getClientIP(req)
	// X-Forwarded-For should take priority
	if got != "203.0.113.10" {
		t.Errorf("expected XFF IP 203.0.113.10 to take priority, got %q", got)
	}
}

// TestGetClientIP_RemoteAddr_NoPort verifies behavior when RemoteAddr has no port.
// net.SplitHostPort returns an error for a bare IP, so the empty string is returned.
func TestGetClientIP_RemoteAddr_NoPort(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(10, 10, false)
	defer rl.Stop()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// Set RemoteAddr without a port — net.SplitHostPort will fail
	req.RemoteAddr = "192.168.1.100"

	// Should not panic; returns whatever net.SplitHostPort yields (empty string on error)
	_ = rl.getClientIP(req)
}

// TestStop_Idempotent verifies that calling Stop() multiple times does not panic.
func TestStop_Idempotent(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(10, 10, false)

	// Should not panic on first call
	rl.Stop()
	// Should not panic on second call
	rl.Stop()
	// Third call for good measure
	rl.Stop()
}

// TestStop_StopsCleanupLoop verifies that after Stop() the stopCh is closed
// and the goroutine can exit cleanly.
func TestStop_StopsCleanupLoop(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(10, 10, false)
	rl.Stop()

	// After Stop() the stopCh should be closed; reading from it should not block.
	select {
	case _, ok := <-rl.stopCh:
		if ok {
			t.Error("expected stopCh to be closed (received value from open channel)")
		}
	default:
		t.Error("expected stopCh to be closed and readable without blocking")
	}
}

// TestAllow_WithTrustProxy_PerIPIsolation verifies per-IP isolation with proxy headers.
func TestAllow_WithTrustProxy_PerIPIsolation(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(1, 1, true)
	defer rl.Stop()

	makeReq := func(xff string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:9090"
		req.Header.Set("X-Forwarded-For", xff)
		return req
	}

	req1 := makeReq("11.11.11.11")
	req2 := makeReq("22.22.22.22")

	// Exhaust client-1
	if !rl.Allow(req1) {
		t.Fatal("client 11.11.11.11 first request should be allowed")
	}
	if rl.Allow(req1) {
		t.Error("client 11.11.11.11 second request should be denied")
	}

	// client-2 is independent
	if !rl.Allow(req2) {
		t.Error("client 22.22.22.22 should be allowed independently")
	}
}

// TestAllow_CreatesLimiterEntry verifies that Allow creates an entry in the limiters map.
func TestAllow_CreatesLimiterEntry(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(10, 10, false)
	defer rl.Stop()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "55.55.55.55:1234"

	rl.mu.RLock()
	initialLen := len(rl.limiters)
	rl.mu.RUnlock()

	rl.Allow(req)

	rl.mu.RLock()
	afterLen := len(rl.limiters)
	rl.mu.RUnlock()

	if afterLen != initialLen+1 {
		t.Errorf("expected limiters map to grow by 1, had %d now %d", initialLen, afterLen)
	}
}

// TestCleanup_RetainsActiveEntry verifies that cleanup() does NOT remove a limiter
// that still has fewer tokens than burst (meaning it was recently used).
func TestCleanup_RetainsActiveEntry(t *testing.T) {
	t.Parallel()
	// burst=5: consume one token so the limiter has 4 tokens (< burst), which cleanup keeps.
	rl := NewRateLimiter(10, 5, false)
	defer rl.Stop()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "77.77.77.77:1234"
	rl.Allow(req) // consumes 1 token; remaining = 4 < burst(5)

	rl.cleanup()

	rl.mu.RLock()
	_, exists := rl.limiters["77.77.77.77"]
	rl.mu.RUnlock()

	if !exists {
		t.Error("expected limiter to be retained after one Allow (tokens < burst)")
	}
}

// TestCleanup_RemovesIdleEntry verifies that a limiter at full burst capacity
// (i.e. never used since it was created, or fully refilled) is removed by cleanup().
// We simulate this by injecting a fresh limiter directly into the map, which starts
// at burst capacity by definition of rate.NewLimiter.
func TestCleanup_RemovesIdleEntry(t *testing.T) {
	t.Parallel()
	// burst=3: a freshly-created limiter has Tokens()==3==burst, so cleanup removes it.
	rl := NewRateLimiter(10, 3, false)
	defer rl.Stop()

	// Inject a fresh limiter directly (simulating an idle IP that was tracked but
	// never consumed any tokens since the last cleanup cycle).
	rl.mu.Lock()
	rl.limiters["idle.ip"] = rate.NewLimiter(rate.Limit(10), 3)
	rl.mu.Unlock()

	// Confirm the entry exists before cleanup.
	rl.mu.RLock()
	_, exists := rl.limiters["idle.ip"]
	rl.mu.RUnlock()
	if !exists {
		t.Fatal("setup: expected idle.ip entry to be injected")
	}

	rl.cleanup()

	rl.mu.RLock()
	_, stillExists := rl.limiters["idle.ip"]
	rl.mu.RUnlock()

	if stillExists {
		t.Error("expected idle entry (tokens==burst) to be removed by cleanup()")
	}
}

// TestRateLimitMiddleware_Returns429WhenExhausted verifies that the middleware
// returns HTTP 429 once the burst is exhausted.
func TestRateLimitMiddleware_Returns429WhenExhausted(t *testing.T) {
	t.Parallel()

	// Create the rate limiter directly with burst=2 so it exhausts quickly.
	rl := NewRateLimiter(1, 2, false)
	defer rl.Stop()

	h := &Handler{
		rateLimiter: rl,
		logger:      zap.NewNop(),
	}

	// Inner handler always returns 200.
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	middleware := h.rateLimitMiddleware(inner)

	allowed := 0
	denied := 0
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/pocket/status", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		w := httptest.NewRecorder()
		middleware.ServeHTTP(w, req)
		switch w.Code {
		case http.StatusOK:
			allowed++
		case http.StatusTooManyRequests:
			denied++
		}
	}

	if allowed == 0 {
		t.Error("expected at least one request to be allowed before burst was exhausted")
	}
	if denied == 0 {
		t.Error("expected at least one request to be denied (429) once burst was exhausted")
	}
}

// TestRateLimitMiddleware_AllowsWhenUnderLimit verifies that requests within burst
// all receive 200 OK.
func TestRateLimitMiddleware_AllowsWhenUnderLimit(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(100, 100, false)
	defer rl.Stop()

	h := &Handler{
		rateLimiter: rl,
		logger:      zap.NewNop(),
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	middleware := h.rateLimitMiddleware(inner)

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.1.2.3:5678"
		w := httptest.NewRecorder()
		middleware.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("request %d: expected 200, got %d", i+1, w.Code)
		}
	}
}
