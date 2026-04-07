package checker

import (
	"strings"
	"testing"
)

// wsURLFrom mirrors the URL-conversion logic inside CheckWebSocket so we can
// test it in isolation without opening a real network connection.
func wsURLFrom(httpURL string) string {
	wsURL := httpURL
	if len(wsURL) > 0 && wsURL[len(wsURL)-1] == '/' {
		wsURL = wsURL[:len(wsURL)-1]
	}
	switch {
	case strings.HasPrefix(wsURL, "http://"):
		wsURL = "ws://" + wsURL[len("http://"):]
	case strings.HasPrefix(wsURL, "https://"):
		wsURL = "wss://" + wsURL[len("https://"):]
	case len(wsURL) > 0 && wsURL[0] != 'w':
		wsURL = "wss://" + wsURL
	}
	wsURL += "/websocket"
	return wsURL
}

// TestWebSocketURLConversion checks that http/https URLs are converted to
// the correct ws/wss scheme and that /websocket is appended.
func TestWebSocketURLConversion(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"http://localhost:26657", "ws://localhost:26657/websocket"},
		{"http://localhost:26657/", "ws://localhost:26657/websocket"},
		{"https://rpc.example.com", "wss://rpc.example.com/websocket"},
		{"https://rpc.example.com/", "wss://rpc.example.com/websocket"},
		// Bare host — assume TLS
		{"rpc.example.com:26657", "wss://rpc.example.com:26657/websocket"},
		// Already a ws:// URL — must pass through unchanged (minus trailing slash)
		{"ws://localhost:26657", "ws://localhost:26657/websocket"},
		{"wss://secure.example.com", "wss://secure.example.com/websocket"},
	}

	for _, tc := range cases {
		got := wsURLFrom(tc.input)
		if got != tc.want {
			t.Errorf("wsURLFrom(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
