package urlutil

import "strings"

// TrimTrailingSlash removes a trailing slash from a URL
func TrimTrailingSlash(url string) string {
	return strings.TrimRight(url, "/")
}

// EnsureHTTPScheme prepends "https://" if the URL has no scheme
func EnsureHTTPScheme(url string) string {
	if url == "" {
		return url
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return url
	}
	return "https://" + url
}

// NormalizeURL trims trailing slash and ensures HTTP scheme
func NormalizeURL(url string) string {
	return EnsureHTTPScheme(TrimTrailingSlash(url))
}

// HTTPToWSScheme converts an HTTP(S) URL to WS(S)
func HTTPToWSScheme(url string) string {
	if strings.HasPrefix(url, "https://") {
		return "wss://" + strings.TrimPrefix(url, "https://")
	}
	if strings.HasPrefix(url, "http://") {
		return "ws://" + strings.TrimPrefix(url, "http://")
	}
	return "wss://" + url
}
