package grpcutil

import "strings"

// ConnKey keys a connection cache by target and transport: a TLS and a
// plaintext connection to the same target are different connections.
func ConnKey(target string, insecure bool) string {
	if insecure {
		return target + "|plaintext"
	}
	return target + "|tls"
}

// ConnTarget returns the target part of a ConnKey.
func ConnTarget(key string) string {
	if i := strings.LastIndexByte(key, '|'); i >= 0 {
		return key[:i]
	}
	return key
}
