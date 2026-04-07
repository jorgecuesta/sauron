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

// CheckNode checks the height of a single node via gRPC.
// The insecure parameter is unused; the node's GRPCInsecure field is used instead.
func (c *GRPCChecker) CheckNode(ctx context.Context, node config.Node) error {
	if node.GRPC == "" {
		return fmt.Errorf("node %s has no gRPC endpoint configured", node.Name)
	}

	// Get or create connection (use per-node grpc_insecure setting)
	conn, err := c.getConnection(node)
	if err != nil {
		recordCheckError(c.logger, node.Network, node.Name, "grpc", "connection", err)
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
		recordCheckError(c.logger, node.Network, node.Name, "grpc", "grpc_call", err)
		metrics.NodeAvailable.WithLabelValues(node.Network, node.Name, "grpc").Set(0)
		return fmt.Errorf("failed to query chain height: %w", err)
	}

	height := resp.Height
	recordCheckSuccess(c.store, c.cache, c.logger, ctx, node.Network, node.Name, "grpc", height, latency)
	return nil
}

// getConnection returns an existing connection or creates a new one.
// LoadOrCompute guarantees the factory runs exactly once per key, eliminating
// the TOCTOU race where two goroutines both see a cache miss and both create connections.
func (c *GRPCChecker) getConnection(node config.Node) (*grpc.ClientConn, error) {
	var createErr error

	conn, _ := c.connections.LoadOrCompute(node.Name, func() (newConn *grpc.ClientConn, cancel bool) {
		newConn, createErr = c.createConnection(node)
		if createErr != nil {
			return nil, true // cancel the store; don't cache a nil value
		}
		return newConn, false
	})

	if createErr != nil {
		return nil, createErr
	}
	if conn == nil {
		return nil, fmt.Errorf("connection creation cancelled for node %s", node.Name)
	}
	return conn, nil
}

// createConnection dials gRPC and warms up the connection.
func (c *GRPCChecker) createConnection(node config.Node) (*grpc.ClientConn, error) {
	var opts []grpc.DialOption
	if node.GRPCInsecure {
		opts = append(opts, grpc.WithTransportCredentials(grpcinsecure.NewCredentials()))
	} else {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	}

	opts = append(opts,
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(100*1024*1024),
			grpc.MaxCallSendMsgSize(100*1024*1024),
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             3 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			MinConnectTimeout: 10 * time.Second,
		}),
	)

	// Use passthrough:/// resolver to avoid DNS resolver IPv6 timeout issues
	target := node.GRPC
	if !strings.HasPrefix(target, "passthrough://") && !strings.HasPrefix(target, "dns://") {
		target = "passthrough:///" + target
	}

	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, err
	}

	// Warm up — best effort, non-blocking
	client := tmservice.NewServiceClient(conn)
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := client.ABCIQuery(warmupCtx, &tmservice.ABCIQueryRequest{
		Path:  "/app/version",
		Data:  []byte{},
		Prove: false,
	}); err != nil {
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
