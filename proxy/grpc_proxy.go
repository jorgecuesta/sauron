package proxy

import (
	"crypto/tls"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"sauron/config"
	"sauron/metrics"
	"sauron/selector"
	"sauron/storage"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

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
	selector      *selector.Selector
	configLoader  *config.Loader
	endpointStore *storage.ExternalEndpointStore
	logger        *zap.Logger
	network       string // The network this proxy serves

	// Connection pool for backend connections (optimization)
	connPool map[string]*grpc.ClientConn
	connMu   sync.RWMutex
}

// NewGRPCProxy creates a new gRPC proxy for a specific network
func NewGRPCProxy(
	selector *selector.Selector,
	configLoader *config.Loader,
	endpointStore *storage.ExternalEndpointStore,
	logger *zap.Logger,
	network string,
) *GRPCProxy {
	return &GRPCProxy{
		selector:      selector,
		configLoader:  configLoader,
		endpointStore: endpointStore,
		logger:        logger,
		network:       network,
		connPool:      make(map[string]*grpc.ClientConn),
	}
}

// GetServer creates a gRPC server configured as a transparent proxy
func (p *GRPCProxy) GetServer() *grpc.Server {
	// Get network config for message size limits
	cfg := p.configLoader.Get()
	var maxRecvSize, maxSendSize int
	for _, network := range cfg.Networks {
		if network.Name == p.network {
			maxRecvSize = network.GRPCMaxRecvMsgSize
			maxSendSize = network.GRPCMaxSendMsgSize
			break
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
	if conn, exists := p.connPool[targetAddr]; exists && conn.GetState().String() != "SHUTDOWN" {
		return conn, nil
	}

	// Create new connection with optimized settings
	var opts []grpc.DialOption
	if useInsecure {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		// Use TLS credentials with system cert pool
		tlsConfig := &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
		creds := credentials.NewTLS(tlsConfig)
		opts = append(opts, grpc.WithTransportCredentials(creds))
	}

	// Get network config for message size limits
	cfg := p.configLoader.Get()
	var maxRecvSize, maxSendSize int
	for _, network := range cfg.Networks {
		if network.Name == p.network {
			maxRecvSize = network.GRPCMaxRecvMsgSize
			maxSendSize = network.GRPCMaxSendMsgSize
			break
		}
	}

	// Default to 100MB if not configured or set to 0
	if maxRecvSize == 0 {
		maxRecvSize = 100 * 1024 * 1024
	}
	if maxSendSize == 0 {
		maxSendSize = 100 * 1024 * 1024
	}

	// Optimization settings
	opts = append(opts,
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxRecvSize), // Use configured limit for backend connections
			grpc.MaxCallSendMsgSize(maxSendSize), // Use configured limit for backend connections
			grpc.ForceCodec(&rawCodec{}),         // Use raw codec for transparent proxying
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second, // Send keepalive pings every 10 seconds
			Timeout:             3 * time.Second,  // Wait 3 seconds for ping ack
			PermitWithoutStream: true,             // Allow pings even with no active streams
		}),
	)

	// Use passthrough:/// resolver to avoid DNS resolver IPv6 timeout issues with Cloudflare
	target := targetAddr
	if !strings.HasPrefix(target, "passthrough://") && !strings.HasPrefix(target, "dns://") {
		target = "passthrough:///" + target
	}

	// Create connection using grpc.NewClient (replaces deprecated DialContext)
	conn, err := grpc.NewClient(target, opts...)
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

	p.logger.Info("gRPC proxy request received",
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

	p.logger.Info("gRPC routing decision made",
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

	// Create client stream
	clientStream, err := conn.NewStream(ctx, &grpc.StreamDesc{
		StreamName:    method,
		ServerStreams: true,
		ClientStreams: true,
	}, method)
	if err != nil {
		p.logger.Error("Failed to create client stream",
			zap.String("method", method),
			zap.Error(err),
		)
		metrics.ProxyErrors.WithLabelValues(p.network, nodeName, "grpc", "unavailable", "stream_error").Inc()
		return status.Errorf(codes.Internal, "failed to create stream: %v", err)
	}

	p.logger.Info("Proxying gRPC to backend",
		zap.String("target", targetAddr),
		zap.String("method", method),
	)

	// Create bidirectional forwarding using raw frames
	// When one goroutine fails, we exit immediately without waiting for both
	errChan := make(chan error, 2)

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
					errChan <- nil
					return
				}
				p.logger.Error("Error receiving from client", zap.Error(err))
				errChan <- fmt.Errorf("recv from client: %w", err)
				return
			}
			p.logger.Debug("Received frame from client", zap.Int("payload_size", len(frame.payload)))

			if err := clientStream.SendMsg(frame); err != nil {
				p.logger.Error("Error sending to backend", zap.Error(err))
				errChan <- fmt.Errorf("send to backend: %w", err)
				return
			}
		}
	}()

	// Forward server -> client
	go func() {
		p.logger.Debug("Started server->client forwarding goroutine")
		defer p.logger.Debug("Exiting server->client forwarding goroutine")

		for {
			frame := &rawFrame{}
			if err := clientStream.RecvMsg(frame); err != nil {
				if err == io.EOF {
					p.logger.Debug("Received EOF from backend")
					errChan <- nil
					return
				}
				p.logger.Error("Error receiving from backend", zap.Error(err))
				errChan <- fmt.Errorf("recv from backend: %w", err)
				return
			}
			p.logger.Debug("Received frame from backend", zap.Int("payload_size", len(frame.payload)))

			if err := stream.SendMsg(frame); err != nil {
				p.logger.Error("Error sending to client", zap.Error(err))
				errChan <- fmt.Errorf("send to client: %w", err)
				return
			}
		}
	}()

	// Wait for completion
	// For normal completion (EOF on both sides), wait for both goroutines
	// For errors, return immediately on first error
	var proxyErr error
	err1 := <-errChan
	if err1 != nil {
		// Got an error (not EOF), return immediately
		p.logger.Debug("First goroutine returned error, exiting immediately", zap.Error(err1))
		proxyErr = err1
	} else {
		// First goroutine completed normally (EOF), wait for second
		p.logger.Debug("First goroutine completed normally, waiting for second...")
		err2 := <-errChan
		if err2 != nil {
			p.logger.Debug("Second goroutine returned error", zap.Error(err2))
			proxyErr = err2
		} else {
			p.logger.Debug("Both goroutines completed normally")
		}
	}

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

	metrics.NodeRequests.WithLabelValues(p.network, nodeName, "grpc", method).Inc()

	if proxyErr != nil {
		metrics.ProxyErrors.WithLabelValues(p.network, nodeName, "grpc", statusStr, "proxy_error").Inc()
		p.logger.Error("gRPC proxy error",
			zap.String("method", method),
			zap.Error(proxyErr),
		)

		// Track 5xx-equivalent gRPC errors for external endpoints
		// gRPC codes that map to 5xx: Internal(13), Unavailable(14), DataLoss(15), Unknown(2)
		if grpcStatus == codes.Internal || grpcStatus == codes.Unavailable ||
			grpcStatus == codes.DataLoss || grpcStatus == codes.Unknown {
			if p.endpointStore != nil {
				if p.endpointStore.TrackProxyError(p.network, "grpc", targetAddr) {
					p.logger.Info("Tracked gRPC 5xx-equivalent error for external endpoint",
						zap.String("addr", targetAddr),
						zap.String("network", p.network),
						zap.String("code", grpcStatus.String()),
					)
				}
			}
		}
	} else {
		p.logger.Info("gRPC request completed",
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

// shouldUseInsecure determines if we should use insecure gRPC connection (network-level)
func (p *GRPCProxy) shouldUseInsecure() bool {
	cfg := p.configLoader.Get()
	for _, network := range cfg.Networks {
		if network.Name == p.network {
			return network.GRPCInsecure
		}
	}
	return false
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
	// If node not found, fall back to network-level setting
	return p.shouldUseInsecure()
}

// Close closes all pooled connections
func (p *GRPCProxy) Close() error {
	p.connMu.Lock()
	defer p.connMu.Unlock()

	for addr, conn := range p.connPool {
		if err := conn.Close(); err != nil {
			p.logger.Warn("Failed to close gRPC connection",
				zap.String("addr", addr),
				zap.Error(err),
			)
		}
	}
	p.connPool = make(map[string]*grpc.ClientConn)
	return nil
}
