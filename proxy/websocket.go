package proxy

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
)

// WSProxyError is returned by ProxyWebSocket with a Category field that
// allows callers to record granular Prometheus error metrics.
type WSProxyError struct {
	// Category matches the original metric error labels:
	// "backend_connect_error", "upgrade_forward_error",
	// "upgrade_response_error", "upgrade_client_error", "websocket_error",
	// "client_disconnect" (the client dropped the connection; not a node failure).
	Category   string
	StatusCode int
	Err        error
}

func (e *WSProxyError) Error() string { return e.Err.Error() }
func (e *WSProxyError) Unwrap() error { return e.Err }

// IsWebSocketRequest checks if an HTTP request is a WebSocket upgrade request.
func IsWebSocketRequest(r *http.Request) bool {
	connection := strings.ToLower(r.Header.Get("Connection"))
	upgrade := strings.ToLower(r.Header.Get("Upgrade"))
	return strings.Contains(connection, "upgrade") && upgrade == "websocket"
}

// ProxyWebSocket handles a WebSocket upgrade by hijacking the client connection,
// dialing the backend, forwarding the upgrade request, and bidirectionally
// copying data until one side closes.
//
// backendURL must be an http:// or https:// URL. The scheme is converted to
// ws:// or wss:// internally. The request path and query string from r are
// appended to the backend URL.
//
// Returns the backend's HTTP response status code (0 if the backend was never
// reached) and any error. Errors are always *WSProxyError with a Category
// field for granular metric recording.
func ProxyWebSocket(w http.ResponseWriter, r *http.Request, backendURL string, logger *zap.Logger) (statusCode int, err error) {
	target, err := url.Parse(backendURL)
	if err != nil {
		return 0, &WSProxyError{Category: "backend_connect_error", Err: err}
	}

	// Hijack the client connection.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return 0, &WSProxyError{Category: "backend_connect_error", Err: io.ErrUnexpectedEOF}
	}

	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		return 0, &WSProxyError{Category: "backend_connect_error", Err: err}
	}
	defer func() { _ = clientConn.Close() }()

	// Build the WebSocket URL for the backend.
	wsScheme := "ws"
	if target.Scheme == "https" {
		wsScheme = "wss"
	}
	wsURL := wsScheme + "://" + target.Host + r.URL.Path
	if r.URL.RawQuery != "" {
		wsURL += "?" + r.URL.RawQuery
	}

	logger.Debug("Connecting to backend WebSocket", zap.String("url", wsURL))

	// Determine the backend address with port.
	backendAddr := target.Host
	if target.Port() == "" {
		if target.Scheme == "https" {
			backendAddr = target.Hostname() + ":443"
		} else {
			backendAddr = target.Hostname() + ":80"
		}
	}

	// Dial the backend.
	var backendConn net.Conn
	if target.Scheme == "https" {
		tlsConfig := &tls.Config{
			ServerName: target.Hostname(),
		}
		backendConn, err = tls.Dial("tcp", backendAddr, tlsConfig)
	} else {
		backendConn, err = net.Dial("tcp", backendAddr)
	}
	if err != nil {
		_, _ = clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return 502, &WSProxyError{Category: "backend_connect_error", StatusCode: 502, Err: err}
	}
	defer func() { _ = backendConn.Close() }()

	// Update the Host header to match the backend.
	r.Host = target.Host
	r.Header.Set("Host", target.Host)

	// Forward the upgrade request.
	if err := r.Write(backendConn); err != nil {
		_, _ = clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return 502, &WSProxyError{Category: "upgrade_forward_error", StatusCode: 502, Err: err}
	}

	// Read the backend's upgrade response.
	backendBuf := bufio.NewReader(backendConn)
	resp, err := http.ReadResponse(backendBuf, r)
	if err != nil {
		_, _ = clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return 502, &WSProxyError{Category: "upgrade_response_error", StatusCode: 502, Err: err}
	}

	// Forward the response to the client.
	if err := resp.Write(clientConn); err != nil {
		return 502, &WSProxyError{Category: "upgrade_client_error", StatusCode: 502, Err: err}
	}

	logger.Debug("WebSocket upgrade successful, starting bidirectional copy",
		zap.Int("status", resp.StatusCode),
	)

	// Bidirectional copy: use the buffered readers (clientBuf, backendBuf)
	// so bytes already read during handshake are not lost.
	// Client->backend records failures reading the client; its source is a
	// bufio reader, so wrapping it costs no zero-copy path. Backend->client is
	// left unwrapped so io.Copy keeps the splice fast path into clientConn.
	errChan := make(chan wsCopyResult, 2)

	go func() {
		src := &clientReadRecorder{r: clientBuf}
		n, copyErr := io.Copy(backendConn, src)
		logger.Debug("Client->Backend copy finished", zap.Int64("bytes", n), zap.Error(copyErr))
		errChan <- wsCopyResult{err: copyErr, clientSide: src.err != nil}
	}()
	go func() {
		n, copyErr := io.Copy(clientConn, backendBuf)
		logger.Debug("Backend->Client copy finished", zap.Int64("bytes", n), zap.Error(copyErr))
		errChan <- wsCopyResult{err: copyErr, backendToClient: true}
	}()

	// Wait for one direction to finish.
	res := <-errChan
	if res.err != nil && res.err != io.EOF {
		clientSide := res.clientSide
		if res.backendToClient {
			// A splice error does not say which socket failed. A client that went
			// away also fails the client->backend read at once; a dead backend
			// leaves that read blocked on a healthy client.
			select {
			case other := <-errChan:
				clientSide = other.clientSide
			case <-time.After(wsClassifyWait):
			}
		}
		category := "websocket_error"
		if clientSide {
			category = "client_disconnect"
		}
		return resp.StatusCode, &WSProxyError{Category: category, StatusCode: resp.StatusCode, Err: res.err}
	}
	return resp.StatusCode, nil
}

// wsClassifyWait bounds how long a failed backend->client copy waits for the
// client->backend side to show whether the client is gone.
const wsClassifyWait = 100 * time.Millisecond

// wsCopyResult is how one copy direction ended.
type wsCopyResult struct {
	err             error
	clientSide      bool // failed reading the client (client->backend only)
	backendToClient bool
}

// clientReadRecorder records a non-EOF error from reading the client.
type clientReadRecorder struct {
	r   io.Reader
	err error
}

func (c *clientReadRecorder) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err != nil && err != io.EOF {
		c.err = err
	}
	return n, err
}
