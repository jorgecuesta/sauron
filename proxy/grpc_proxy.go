package proxy

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"sauron/config"
	"sauron/internal/grpcutil"
	"sauron/metrics"
	"sauron/selector"
	"sauron/storage"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Where a gRPC call's error came from; also the ProxyErrors "reason" label.
const (
	originBackend = "backend_status" // a status the backend returned, relayed as-is
	originClient  = "client_error"   // the client's stream ended or failed
	originProxy   = "proxy_error"    // a failure of the proxy itself
)

// trackableBackendFailure reports whether a call's end may count against the
// endpoint's health: a 5xx-equivalent code (Internal, Unavailable, DataLoss,
// Unknown) that did not come from the client going away.
func trackableBackendFailure(origin string, code codes.Code) bool {
	if origin == originClient {
		return false
	}
	switch code {
	case codes.Internal, codes.Unavailable, codes.DataLoss, codes.Unknown:
		return true
	}
	return false
}

// callEnd is how one forwarding direction ended; err is nil on a clean end.
type callEnd struct {
	err    error
	origin string
}

// rawFrame represents a raw gRPC frame for transparent proxying
type rawFrame struct {
	payload []byte
}

// rawCodec implements a codec that simply passes through raw bytes
// This enables transparent proxying without needing to know the proto types
type rawCodec struct{}

func (c *rawCodec) Marshal(v interface{}) ([]byte, error) {
	if frame, ok := v.(*rawFrame); ok {
		return frame.payload, nil
	}
	return nil, fmt.Errorf("invalid type for raw codec: %T", v)
}

func (c *rawCodec) Unmarshal(data []byte, v interface{}) error {
	if frame, ok := v.(*rawFrame); ok {
		frame.payload = data
		return nil
	}
	return fmt.Errorf("invalid type for raw codec: %T", v)
}

func (c *rawCodec) Name() string {
	return "raw"
}

func init() {
	encoding.RegisterCodec(&rawCodec{})
}

// GRPCProxy handles gRPC proxying with transparent request forwarding
// The Eye's gaze through the gRPC realm
type GRPCProxy struct {
	selector       *selector.Selector
	configLoader   *config.MultiChainLoader
	endpointStore  *storage.ExternalEndpointStore
	circuitBreaker *CircuitBreaker
	logger         *zap.Logger
	network        string // The network this proxy serves

	// Connection pool for backend connections (optimization)
	connPool  map[string]*grpc.ClientConn
	connMu    sync.RWMutex
	closeOnce sync.Once
}

// NewGRPCProxy creates a new gRPC proxy for a specific network
func NewGRPCProxy(
	selector *selector.Selector,
	configLoader *config.MultiChainLoader,
	endpointStore *storage.ExternalEndpointStore,
	logger *zap.Logger,
	network string,
	circuitBreaker *CircuitBreaker,
) *GRPCProxy {
	return &GRPCProxy{
		selector:       selector,
		configLoader:   configLoader,
		endpointStore:  endpointStore,
		circuitBreaker: circuitBreaker,
		logger:         logger,
		network:        network,
		connPool:       make(map[string]*grpc.ClientConn),
	}
}

// GetServer creates a gRPC server configured as a transparent proxy
func (p *GRPCProxy) GetServer() *grpc.Server {
	// Get network config for message size limits
	cfg := p.configLoader.Get()
	var maxRecvSize, maxSendSize int
	net := cfg.FindNetwork(p.network)
	if net != nil {
		if ep := net.FindEndpoint("grpc"); ep != nil {
			maxRecvSize = ep.GRPCMaxRecvMsgSize
			maxSendSize = ep.GRPCMaxSendMsgSize
		}
	}

	// Default to 100MB if not configured or set to 0
	if maxRecvSize == 0 {
		maxRecvSize = 100 * 1024 * 1024
	}
	if maxSendSize == 0 {
		maxSendSize = 100 * 1024 * 1024
	}

	// Create gRPC server with unknown service handler for transparent proxying
	opts := []grpc.ServerOption{
		grpc.UnknownServiceHandler(p.proxyHandler),
		grpc.MaxRecvMsgSize(maxRecvSize),
		grpc.MaxSendMsgSize(maxSendSize),
		grpc.ForceServerCodec(&rawCodec{}), // Use raw codec for transparent proxying
	}

	server := grpc.NewServer(opts...)
	return server
}

// getOrCreateConnection gets a pooled connection or creates a new one (optimization)
func (p *GRPCProxy) getOrCreateConnection(targetAddr string, useInsecure bool) (*grpc.ClientConn, error) {
	// Check if we have a cached connection
	p.connMu.RLock()
	if conn, exists := p.connPool[targetAddr]; exists {
		// Verify connection is still valid
		if conn.GetState().String() != "SHUTDOWN" {
			p.connMu.RUnlock()
			return conn, nil
		}
	}
	p.connMu.RUnlock()

	// Need to create new connection
	p.connMu.Lock()
	defer p.connMu.Unlock()

	// Double-check after acquiring write lock
	if conn, exists := p.connPool[targetAddr]; exists {
		if conn.GetState().String() != "SHUTDOWN" {
			return conn, nil
		}
		// Connection is in SHUTDOWN state — close it to release resources before replacing
		if err := conn.Close(); err != nil {
			p.logger.Warn("Failed to close SHUTDOWN gRPC connection",
				zap.String("addr", targetAddr),
				zap.Error(err),
			)
		}
		delete(p.connPool, targetAddr)
	}

	// Get network config for message size limits
	cfg := p.configLoader.Get()
	var maxRecvSize, maxSendSize int
	net2 := cfg.FindNetwork(p.network)
	if net2 != nil {
		if ep := net2.FindEndpoint("grpc"); ep != nil {
			maxRecvSize = ep.GRPCMaxRecvMsgSize
			maxSendSize = ep.GRPCMaxSendMsgSize
		}
	}

	// Default to 100MB if not configured or set to 0
	if maxRecvSize == 0 {
		maxRecvSize = 100 * 1024 * 1024
	}
	if maxSendSize == 0 {
		maxSendSize = 100 * 1024 * 1024
	}

	ka := keepalive.ClientParameters{
		Time:                cfg.GRPCKeepalive.Time,
		Timeout:             cfg.GRPCKeepalive.Timeout,
		PermitWithoutStream: cfg.GRPCKeepalive.PermitWithoutStream,
	}

	conn, err := grpcutil.NewConnection(targetAddr, useInsecure, ka,
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxRecvSize), // Use configured limit for backend connections
			grpc.MaxCallSendMsgSize(maxSendSize), // Use configured limit for backend connections
			grpc.ForceCodec(&rawCodec{}),         // Use raw codec for transparent proxying
		),
	)
	if err != nil {
		return nil, err
	}

	p.connPool[targetAddr] = conn
	return conn, nil
}

// proxyHandler handles all incoming gRPC requests and forwards them
func (p *GRPCProxy) proxyHandler(srv interface{}, stream grpc.ServerStream) error {
	start := time.Now()

	// Get method name from stream context
	method, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		p.logger.Error("Failed to get method from stream")
		return status.Errorf(codes.Internal, "failed to get method name")
	}

	p.logger.Debug("gRPC proxy request received",
		zap.String("method", method),
		zap.String("network", p.network),
	)

	// Select best node
	nodeMetrics, nodeName, decision := p.selector.GetBestNode(p.network, "grpc")
	if nodeMetrics == nil || nodeName == "" {
		p.logger.Warn("No available nodes for gRPC routing",
			zap.String("network", p.network),
		)
		return status.Errorf(codes.Unavailable, "no available nodes")
	}

	// Get endpoint URL
	targetAddr := p.selector.GetEndpointURL(nodeName, "grpc")
	if targetAddr == "" {
		p.logger.Error("Failed to get gRPC endpoint",
			zap.String("node", nodeName),
		)
		return status.Errorf(codes.Internal, "failed to get endpoint")
	}

	p.logger.Debug("gRPC routing decision made",
		zap.String("network", p.network),
		zap.String("selected_node", nodeName),
		zap.String("target", targetAddr),
		zap.String("method", method),
	)

	// Determine if we should use insecure connection for THIS node
	useInsecure := p.shouldUseInsecureForNode(nodeName)

	// Get or create pooled connection (optimization)
	conn, err := p.getOrCreateConnection(targetAddr, useInsecure)
	if err != nil {
		p.logger.Error("Failed to dial backend",
			zap.String("target", targetAddr),
			zap.Error(err),
		)
		metrics.ProxyErrors.WithLabelValues(p.network, nodeName, "grpc", "unavailable", "dial_error").Inc()
		return status.Errorf(codes.Unavailable, "failed to connect to backend: %v", err)
	}

	// Forward metadata
	ctx := stream.Context()
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		ctx = metadata.NewOutgoingContext(ctx, md)
	}
	// Cancelled before returning so the server->client goroutine can be joined.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Create client stream
	clientStream, err := conn.NewStream(ctx, &grpc.StreamDesc{
		StreamName:    method,
		ServerStreams: true,
		ClientStreams: true,
	}, method)
	if err != nil {
		if clientErr := stream.Context().Err(); clientErr != nil {
			// The client left while the backend stream was being opened: not
			// the endpoint's fault. Recorded like any other finished call.
			clientStatus := status.FromContextError(clientErr)
			statusStr := strconv.Itoa(int(clientStatus.Code()))
			metrics.ProxyRequestDuration.WithLabelValues(p.network, nodeName, "grpc", statusStr).Observe(time.Since(start).Seconds())
			metrics.NodeRequests.WithLabelValues(p.network, nodeName, "grpc", normalizeGRPCMethod(method)).Inc()
			metrics.ProxyErrors.WithLabelValues(p.network, nodeName, "grpc", statusStr, originClient).Inc()
			p.logger.Debug("Client cancelled during gRPC stream setup",
				zap.String("method", method),
				zap.Error(clientErr),
			)
			return clientStatus.Err()
		}
		p.logger.Error("Failed to create client stream",
			zap.String("method", method),
			zap.Error(err),
		)
		metrics.ProxyErrors.WithLabelValues(p.network, nodeName, "grpc", "unavailable", "stream_error").Inc()
		// The backend cannot be reached: that is Unavailable (clients retry on
		// it), not Internal, and it counts against the endpoint like any other
		// backend failure. Dial details stay in the log.
		if p.endpointStore != nil {
			p.endpointStore.TrackProxyError(p.network, "grpc", targetAddr)
		}
		return status.Error(codes.Unavailable, "backend unavailable")
	}

	p.logger.Debug("Proxying gRPC to backend",
		zap.String("target", targetAddr),
		zap.String("method", method),
	)

	// Create bidirectional forwarding using raw frames.
	// The backend side decides when the call ends: its status ends the call even
	// if the client never half-closed. A client-side failure ends it at once.
	c2sEnd := make(chan callEnd, 1)
	s2cEnd := make(chan callEnd, 1)

	// Forward client -> server
	go func() {
		p.logger.Debug("Started client->server forwarding goroutine")
		defer p.logger.Debug("Exiting client->server forwarding goroutine")

		for {
			frame := &rawFrame{}
			if err := stream.RecvMsg(frame); err != nil {
				if err == io.EOF {
					p.logger.Debug("Received EOF from client, closing send")
					_ = clientStream.CloseSend()
					c2sEnd <- callEnd{}
					return
				}
				if status.Code(err) == codes.ResourceExhausted {
					// Rejected by this proxy's own grpc_max_recv_msg_size (grpc-go
					// also cancels the stream then): a configuration limit operators
					// must see, not a client fault.
					p.logger.Error("Request exceeds the proxy's receive limit", zap.Error(err))
					c2sEnd <- callEnd{err: err, origin: originProxy}
					return
				}
				p.logger.Debug("Client stream ended", zap.Error(err))
				c2sEnd <- callEnd{err: err, origin: originClient}
				return
			}
			p.logger.Debug("Received frame from client", zap.Int("payload_size", len(frame.payload)))

			if err := clientStream.SendMsg(frame); err != nil {
				if err == io.EOF {
					// The backend ended the stream; its status arrives via RecvMsg
					// in the server->client goroutine.
					c2sEnd <- callEnd{}
					return
				}
				if stream.Context().Err() != nil {
					c2sEnd <- callEnd{err: err, origin: originClient}
					return
				}
				p.logger.Error("Error sending to backend", zap.Error(err))
				c2sEnd <- callEnd{err: err, origin: originProxy}
				return
			}
		}
	}()

	// Forward server -> client
	s2cDone := make(chan struct{})
	go func() {
		defer close(s2cDone)
		p.logger.Debug("Started server->client forwarding goroutine")
		defer p.logger.Debug("Exiting server->client forwarding goroutine")

		// Header blocks until the backend sends headers. It returns nil when the
		// backend ended without any (trailers-only); the status and trailer then
		// carry everything, so nothing is set and the reply keeps that shape.
		if md, _ := clientStream.Header(); md != nil {
			_ = stream.SetHeader(md)
		}

		for {
			frame := &rawFrame{}
			if err := clientStream.RecvMsg(frame); err != nil {
				stream.SetTrailer(clientStream.Trailer())
				if err == io.EOF {
					p.logger.Debug("Received EOF from backend")
					s2cEnd <- callEnd{}
					return
				}
				origin := originBackend
				if stream.Context().Err() != nil {
					// The backend call was cancelled because the client went away.
					origin = originClient
				}
				p.logger.Debug("Backend stream ended", zap.String("origin", origin), zap.Error(err))
				s2cEnd <- callEnd{err: err, origin: origin}
				return
			}
			p.logger.Debug("Received frame from backend", zap.Int("payload_size", len(frame.payload)))

			if err := stream.SendMsg(frame); err != nil {
				p.logger.Debug("Client went away while sending", zap.Error(err))
				s2cEnd <- callEnd{err: err, origin: originClient}
				return
			}
		}
	}()

	// Wait for completion
	var end callEnd
	clientSideFailed := false
	select {
	case end = <-s2cEnd:
		// The backend finished (or the client went away): the call is over.
	case end = <-c2sEnd:
		if end.err == nil {
			// Client half-closed, or the backend ended mid-send: its status follows.
			end = <-s2cEnd
		} else {
			clientSideFailed = true
		}
	}
	cancel()
	// Wait for the server->client goroutine so it never touches the server
	// stream after this handler returns. The one exception: the client side
	// failed while the client's stream is still open (a client that stopped
	// reading); the goroutine may then be blocked in SendMsg on flow control,
	// and only returning (which ends the server stream) unblocks it.
	if !clientSideFailed || stream.Context().Err() != nil {
		<-s2cDone
	}
	// Returned as-is: every error here is already a gRPC status (or becomes
	// Unknown), and wrapping it would rewrite the message the client sees.
	proxyErr := end.err

	// Record metrics
	duration := time.Since(start)
	grpcStatus := status.Code(proxyErr)
	statusStr := strconv.Itoa(int(grpcStatus))

	metrics.ProxyRequestDuration.WithLabelValues(
		p.network,
		nodeName,
		"grpc",
		statusStr,
	).Observe(duration.Seconds())

	metrics.NodeRequests.WithLabelValues(p.network, nodeName, "grpc", normalizeGRPCMethod(method)).Inc()

	if proxyErr != nil {
		metrics.ProxyErrors.WithLabelValues(p.network, nodeName, "grpc", statusStr, end.origin).Inc()
		// Backend statuses (e.g. NotFound) and clients going away are routine
		// per-request outcomes; only failures of the proxy itself are errors.
		if end.origin == originProxy {
			p.logger.Error("gRPC proxy error",
				zap.String("method", method),
				zap.Error(proxyErr),
			)
		} else {
			p.logger.Debug("gRPC call ended with status",
				zap.String("method", method),
				zap.String("origin", end.origin),
				zap.Error(proxyErr),
			)
		}

		// Track 5xx-equivalent gRPC errors for external endpoints.
		if trackableBackendFailure(end.origin, grpcStatus) {
			if p.endpointStore != nil {
				if p.endpointStore.TrackProxyError(p.network, "grpc", targetAddr) {
					p.logger.Debug("Tracked gRPC 5xx-equivalent error for external endpoint",
						zap.String("addr", targetAddr),
						zap.String("network", p.network),
						zap.String("code", grpcStatus.String()),
					)
				}
			}
		}
	} else {
		p.logger.Debug("gRPC request completed",
			zap.String("method", method),
			zap.Duration("duration", duration),
		)
	}

	p.logger.Debug("gRPC request proxied",
		zap.String("network", p.network),
		zap.String("node", nodeName),
		zap.String("method", method),
		zap.Duration("duration", duration),
		zap.String("selection_reason", decision.Reason),
	)

	return proxyErr
}

// shouldUseInsecureForNode determines if we should use insecure connection for a specific node
func (p *GRPCProxy) shouldUseInsecureForNode(nodeName string) bool {
	cfg := p.configLoader.Get()
	// Check internal nodes
	for _, node := range cfg.Internals {
		if node.Name == nodeName {
			return node.GRPCInsecure
		}
	}
	return false
}

// normalizeGRPCMethod extracts the service name from a full gRPC method path.
// "/package.Service/Method" → "package.Service"
// Returns the input unchanged when the format is not recognised.
func normalizeGRPCMethod(method string) string {
	// Strip leading slash
	s := strings.TrimPrefix(method, "/")
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		return s[:idx]
	}
	return s
}

// Close closes all pooled connections. It is idempotent; subsequent calls are no-ops.
func (p *GRPCProxy) Close() error {
	var closeErr error
	p.closeOnce.Do(func() {
		p.connMu.Lock()
		defer p.connMu.Unlock()

		for addr, conn := range p.connPool {
			if err := conn.Close(); err != nil {
				p.logger.Warn("Failed to close gRPC connection",
					zap.String("addr", addr),
					zap.Error(err),
				)
				closeErr = err
			}
		}
		p.connPool = make(map[string]*grpc.ClientConn)
	})
	return closeErr
}
