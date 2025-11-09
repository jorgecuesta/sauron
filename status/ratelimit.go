package status

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter manages per-IP rate limiting using token bucket algorithm
type RateLimiter struct {
	limiters      map[string]*rate.Limiter
	mu            sync.RWMutex
	requestsPerIP int          // requests per time window
	burst         int          // burst capacity
	trustProxy    bool         // whether to trust X-Forwarded-For and similar headers
	cleanupTicker *time.Ticker // periodic cleanup of old limiters
}

// NewRateLimiter creates a new rate limiter
// requestsPerIP: number of requests allowed per second per IP
// burst: maximum burst size (should be >= requestsPerIP)
// trustProxy: if true, trust proxy headers (X-Forwarded-For, etc.)
func NewRateLimiter(requestsPerIP int, burst int, trustProxy bool) *RateLimiter {
	rl := &RateLimiter{
		limiters:      make(map[string]*rate.Limiter),
		requestsPerIP: requestsPerIP,
		burst:         burst,
		trustProxy:    trustProxy,
	}

	// Start cleanup goroutine to prevent memory leaks
	rl.cleanupTicker = time.NewTicker(5 * time.Minute)
	go rl.cleanupLoop()

	return rl
}

// Allow checks if a request from the given IP should be allowed
func (rl *RateLimiter) Allow(r *http.Request) bool {
	ip := rl.getClientIP(r)

	rl.mu.Lock()
	limiter, exists := rl.limiters[ip]
	if !exists {
		limiter = rate.NewLimiter(rate.Limit(rl.requestsPerIP), rl.burst)
		rl.limiters[ip] = limiter
	}
	rl.mu.Unlock()

	return limiter.Allow()
}

// getClientIP extracts the real client IP from the request
// This handles various proxy scenarios (HAProxy, Nginx, Cloudflare, etc.)
func (rl *RateLimiter) getClientIP(r *http.Request) string {
	// If not trusting proxy headers, use RemoteAddr directly
	if !rl.trustProxy {
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		return ip
	}

	// Check headers in priority order when behind proxies
	// X-Forwarded-For: Contains chain of IPs (client, proxy1, proxy2, ...)
	// We want the leftmost (original client) IP
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For can be: "client, proxy1, proxy2"
		// Take the first IP (leftmost = original client)
		ips := strings.Split(xff, ",")
		if len(ips) > 0 {
			clientIP := strings.TrimSpace(ips[0])
			if ip := net.ParseIP(clientIP); ip != nil {
				return clientIP
			}
		}
	}

	// X-Real-IP: Used by Nginx, contains single IP
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		if ip := net.ParseIP(xri); ip != nil {
			return xri
		}
	}

	// CF-Connecting-IP: Cloudflare's header for the real client IP
	if cfip := r.Header.Get("CF-Connecting-IP"); cfip != "" {
		if ip := net.ParseIP(cfip); ip != nil {
			return cfip
		}
	}

	// True-Client-IP: Another Cloudflare header
	if tcip := r.Header.Get("True-Client-IP"); tcip != "" {
		if ip := net.ParseIP(tcip); ip != nil {
			return tcip
		}
	}

	// Fallback to RemoteAddr if no valid proxy headers found
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

// cleanupLoop periodically removes inactive limiters to prevent memory leaks
func (rl *RateLimiter) cleanupLoop() {
	for range rl.cleanupTicker.C {
		rl.cleanup()
	}
}

// cleanup removes limiters that haven't been used recently
func (rl *RateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Remove limiters with no tokens reserved (inactive)
	for ip, limiter := range rl.limiters {
		// If limiter would allow a burst, it's been inactive
		if limiter.Tokens() >= float64(rl.burst) {
			delete(rl.limiters, ip)
		}
	}
}

// Stop stops the cleanup goroutine
func (rl *RateLimiter) Stop() {
	if rl.cleanupTicker != nil {
		rl.cleanupTicker.Stop()
	}
}
