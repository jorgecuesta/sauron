package urlutil_test

import (
	"testing"

	"sauron/internal/urlutil"
)

func TestTrimTrailingSlash(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"no slash", "https://example.com", "https://example.com"},
		{"single slash", "https://example.com/", "https://example.com"},
		{"multiple slashes", "https://example.com///", "https://example.com"},
		{"path with slash", "https://example.com/path/", "https://example.com/path"},
		{"path no slash", "https://example.com/path", "https://example.com/path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := urlutil.TrimTrailingSlash(tc.input)
			if got != tc.want {
				t.Errorf("TrimTrailingSlash(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestEnsureHTTPScheme(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"already https", "https://example.com", "https://example.com"},
		{"already http", "http://example.com", "http://example.com"},
		{"no scheme", "example.com", "https://example.com"},
		// The old bug: hosts starting with 'h' were incorrectly skipped by url[0] != 'h' check
		{"host starts with h", "host.example.com", "https://host.example.com"},
		{"host is h alone", "h", "https://h"},
		{"host starts with https literally no colon", "httpsfake.com", "https://httpsfake.com"},
		{"host starts with http literally no colon", "httpfake.com", "https://httpfake.com"},
		{"with port no scheme", "example.com:9090", "https://example.com:9090"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := urlutil.EnsureHTTPScheme(tc.input)
			if got != tc.want {
				t.Errorf("EnsureHTTPScheme(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"https with slash", "https://example.com/", "https://example.com"},
		{"no scheme no slash", "example.com", "https://example.com"},
		{"no scheme with slash", "example.com/", "https://example.com"},
		// Old bug: host starting with 'h' — must add scheme
		{"host starts with h with slash", "host.example.com/", "https://host.example.com"},
		{"already normalized", "https://example.com", "https://example.com"},
		{"http with trailing slash", "http://example.com/", "http://example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := urlutil.NormalizeURL(tc.input)
			if got != tc.want {
				t.Errorf("NormalizeURL(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestHTTPToWSScheme(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"https to wss", "https://example.com", "wss://example.com"},
		{"http to ws", "http://example.com", "ws://example.com"},
		{"no scheme to wss", "example.com", "wss://example.com"},
		{"https with path", "https://example.com/path", "wss://example.com/path"},
		{"http with port", "http://example.com:26657", "ws://example.com:26657"},
		{"https with port", "https://example.com:443", "wss://example.com:443"},
		// Hosts starting with 'h' — must not be double-prepended
		{"host starts with h no scheme", "host.example.com", "wss://host.example.com"},
		{"empty", "", "wss://"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := urlutil.HTTPToWSScheme(tc.input)
			if got != tc.want {
				t.Errorf("HTTPToWSScheme(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
