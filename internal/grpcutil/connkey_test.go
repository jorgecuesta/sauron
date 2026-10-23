package grpcutil

import "testing"

func TestConnKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		target   string
		insecure bool
		want     string
	}{
		{"node:9090", true, "node:9090|plaintext"},
		{"node:9090", false, "node:9090|tls"},
		{"dns:///node:443", false, "dns:///node:443|tls"},
		{"[::1]:9090", true, "[::1]:9090|plaintext"},
		{"", false, "|tls"},
	}
	for _, tt := range tests {
		key := ConnKey(tt.target, tt.insecure)
		if key != tt.want {
			t.Errorf("ConnKey(%q, %v) = %q, want %q", tt.target, tt.insecure, key, tt.want)
		}
		if got := ConnTarget(key); got != tt.target {
			t.Errorf("ConnTarget(%q) = %q, want %q", key, got, tt.target)
		}
	}
	if ConnKey("a:1", true) == ConnKey("a:1", false) {
		t.Error("TLS and plaintext keys must differ")
	}
	// A string that is not a key comes back unchanged.
	if got := ConnTarget("node:9090"); got != "node:9090" {
		t.Errorf("ConnTarget of a bare target = %q", got)
	}
}
