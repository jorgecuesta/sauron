package checker

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"time"

	"sauron/config"
	"sauron/metrics"
	"sauron/storage"

	tmservice "cosmossdk.io/api/cosmos/base/tendermint/v1beta1"
	"github.com/puzpuzpuz/xsync/v4"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	grpcinsecure "google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// GRPCChecker checks node heights via CosmosSDK gRPC
// The Eye speaking in the ancient protocols
type GRPCChecker struct {
	store       *storage.HeightStore
	cache       *storage.Cache
	logger      *zap.Logger
	connections *xsync.Map[string, *grpc.ClientConn] // node name -> connection
}

// NewGRPCChecker creates a new gRPC checker
func NewGRPCChecker(store *storage.HeightStore, cache *storage.Cache, logger *zap.Logger) *GRPCChecker {
	return &GRPCChecker{
		store:       store,
		cache:       cache,
		logger:      logger,
		connections: xsync.NewMap[string, *grpc.ClientConn](),
	}
}

// CheckNode checks the height of a single node via gRPC
func (c *GRPCChecker) CheckNode(ctx context.Context, node config.Node, insecure bool) error {
	if node.GRPC == "" {
		return fmt.Errorf("node %s has no gRPC endpoint configured", node.Name)
	}

	// Get or create connection (use per-node grpc_insecure setting)
	conn, err := c.getConnection(node, node.GRPCInsecure)
	if err != nil {
		c.recordError(node, "connection", err)
		metrics.NodeAvailable.WithLabelValues(node.Network, node.Name, "grpc").Set(0)
		return fmt.Errorf("failed to connect: %w", err)
	}

	// Create service client
	client := tmservice.NewServiceClient(conn)

	start := time.Now()
	// ABCIQuery with /app/version is the lightest query (~80 bytes vs 5MB for GetLatestBlock)
	// Response includes height field regardless of query path
	resp, err := client.ABCIQuery(ctx, &tmservice.ABCIQueryRequest{
		Path:   "/app/version", // Minimal query that returns version + height
		Data:   []byte{},       // Empty data
		Height: 0,              // 0 = latest height
		Prove:  false,
	})
	latency := time.Since(start)

	if err != nil {
		c.recordError(node, "grpc_call", err)
		metrics.NodeAvailable.WithLabelValues(node.Network, node.Name, "grpc").Set(0)
		return fmt.Errorf("failed to query chain height: %w", err)
	}

	height := resp.Height

	// Update storage
	c.store.Update(node.Network, node.Name, "grpc", height, latency, "internal")

	// Update cache if enabled
	if c.cache.IsEnabled() {
		c.cache.SetHeight(ctx, node.Network, node.Name, "grpc", height, 30*time.Second)
		c.cache.SetLatency(ctx, node.Network, node.Name, "grpc", latency, 30*time.Second)
	}

	// Update metrics
	metrics.NodeHeight.WithLabelValues(node.Network, node.Name, "grpc", "internal").Set(float64(height))
	metrics.NodeLatency.WithLabelValues(node.Network, node.Name, "grpc").Observe(latency.Seconds())
	metrics.NodeAvailable.WithLabelValues(node.Network, node.Name, "grpc").Set(1)
	metrics.HeightCheckDuration.WithLabelValues(node.Network, node.Name, "grpc").Observe(latency.Seconds())

	c.logger.Debug("gRPC height check successful",
		zap.String("node", node.Name),
		zap.String("network", node.Network),
		zap.Int64("height", height),
		zap.Duration("latency", latency),
	)

	return nil
}

// getConnection returns an existing connection or creates a new one
func (c *GRPCChecker) getConnection(node config.Node, insecure bool) (*grpc.ClientConn, error) {
	// Check if we already have a connection
	if conn, exists := c.connections.Load(node.Name); exists {
		return conn, nil
	}

	// Create new connection with proper credentials and optimizations
	var opts []grpc.DialOption
	if insecure {
		// Use insecure credentials (no TLS)
		opts = append(opts, grpc.WithTransportCredentials(grpcinsecure.NewCredentials()))
	} else {
		// Use TLS credentials with system cert pool
		tlsConfig := &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
		creds := credentials.NewTLS(tlsConfig)
		opts = append(opts, grpc.WithTransportCredentials(creds))
	}

	// Add optimization settings: keepalive for connection reuse and connection params
	opts = append(opts,
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(100*1024*1024), // 100MB - handle large messages
			grpc.MaxCallSendMsgSize(100*1024*1024), // 100MB
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second, // Send keepalive pings every 10 seconds
			Timeout:             3 * time.Second,  // Wait 3 seconds for ping ack
			PermitWithoutStream: true,             // Allow pings even with no active streams
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			MinConnectTimeout: 10 * time.Second, // Give connection time to establish
		}),
	)

	// Use passthrough:/// resolver to avoid DNS resolver IPv6 timeout issues with Cloudflare
	target := node.GRPC
	if !strings.HasPrefix(target, "passthrough://") && !strings.HasPrefix(target, "dns://") {
		target = "passthrough:///" + target
	}

	// Create connection using grpc.NewClient (replaces deprecated DialContext)
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, err
	}

	// Warm up the connection by making a test RPC call (best effort, non-blocking)
	// This is an optimization to force connection establishment immediately
	// If it fails, we still return the connection and let the first health check establish it
	client := tmservice.NewServiceClient(conn)
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err = client.ABCIQuery(warmupCtx, &tmservice.ABCIQueryRequest{
		Path:   "/app/version", // Same lightweight query for warmup
		Data:   []byte{},
		Height: 0,
		Prove:  false,
	})
	if err != nil {
		// Warmup failed (likely slow network or TLS negotiation timeout)
		// Log warning but continue - connection will be established on first real health check
		c.logger.Warn("gRPC connection warmup failed, will establish on first health check",
			zap.String("node", node.Name),
			zap.String("url", node.GRPC),
			zap.Error(err),
		)
	} else {
		c.logger.Debug("gRPC connection established and warmed up",
			zap.String("node", node.Name),
			zap.String("url", node.GRPC),
		)
	}

	c.connections.Store(node.Name, conn)
	return conn, nil
}

// Close closes all gRPC connections
func (c *GRPCChecker) Close() error {
	c.connections.Range(func(name string, conn *grpc.ClientConn) bool {
		if err := conn.Close(); err != nil {
			c.logger.Warn("Failed to close gRPC connection",
				zap.String("node", name),
				zap.Error(err),
			)
		}
		return true // continue iteration
	})
	c.connections.Clear()
	return nil
}

func (c *GRPCChecker) recordError(node config.Node, errorType string, err error) {
	metrics.HeightCheckErrors.WithLabelValues(node.Network, node.Name, "grpc", errorType).Inc()
	c.logger.Warn("gRPC height check failed",
		zap.String("node", node.Name),
		zap.String("network", node.Network),
		zap.String("error_type", errorType),
		zap.Error(err),
	)
}
