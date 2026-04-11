package checker

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"sauron/adapter"
	"sauron/config"
	"sauron/internal/grpcutil"
	"sauron/internal/urlutil"
	"sauron/metrics"
	"sauron/storage"

	"github.com/alitto/pond/v2"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/keepalive"
)

// MultiChainScheduler coordinates periodic health and height checks using the adapter engine.
// It replaces the hardcoded API/RPC/gRPC checkers with a single engine that
// executes CheckConfig produced by adapter factories.
type MultiChainScheduler struct {
	cron          *cron.Cron
	pool          pond.Pool
	engine        *adapter.Engine
	registry      *adapter.Registry
	heightStore   *storage.HeightStore
	healthStore   *storage.HealthStore
	cache         *storage.Cache
	endpointStore *storage.ExternalEndpointStore
	extChecker    *ExternalChecker
	oracleChecker *OracleChecker
	archivalStore *storage.ArchivalStore
	configLoader  *config.MultiChainLoader
	logger        *zap.Logger
	timeout       time.Duration
}

// NewMultiChainScheduler creates a scheduler with adapter-driven checks.
func NewMultiChainScheduler(
	engine *adapter.Engine,
	registry *adapter.Registry,
	heightStore *storage.HeightStore,
	healthStore *storage.HealthStore,
	archivalStore *storage.ArchivalStore,
	cache *storage.Cache,
	endpointStore *storage.ExternalEndpointStore,
	oracleChecker *OracleChecker,
	configLoader *config.MultiChainLoader,
	pool pond.Pool,
	logger *zap.Logger,
) *MultiChainScheduler {
	extChecker := NewExternalChecker(heightStore, endpointStore, configLoader, logger)

	return &MultiChainScheduler{
		cron: cron.New(
			cron.WithSeconds(),
			cron.WithChain(cron.Recover(cron.DefaultLogger)),
		),
		pool:          pool,
		engine:        engine,
		registry:      registry,
		heightStore:   heightStore,
		healthStore:   healthStore,
		cache:         cache,
		endpointStore: endpointStore,
		extChecker:    extChecker,
		oracleChecker: oracleChecker,
		archivalStore: archivalStore,
		configLoader:  configLoader,
		logger:        logger,
		timeout:       5 * time.Second,
	}
}

// Start begins scheduled checks. Each network gets its own height check
// interval based on config; health checks and externals use fixed intervals.
func (s *MultiChainScheduler) Start() error {
	cfg := s.configLoader.Get()
	s.timeout = cfg.Timeouts.HealthCheck

	// Schedule internal node checks every 30 seconds.
	_, err := s.cron.AddFunc("*/30 * * * * *", func() {
		s.checkInternalNodes()
	})
	if err != nil {
		return fmt.Errorf("multi-chain scheduler: failed to add internal check cron: %w", err)
	}

	// Schedule external ring checks every 10 seconds.
	_, err = s.cron.AddFunc("*/10 * * * * *", func() {
		s.checkExternalRings()
	})
	if err != nil {
		return fmt.Errorf("multi-chain scheduler: failed to add external check cron: %w", err)
	}

	// Schedule recovery every 10 seconds.
	_, err = s.cron.AddFunc("*/10 * * * * *", func() {
		s.recoverFailedEndpoints()
	})
	if err != nil {
		return fmt.Errorf("multi-chain scheduler: failed to add recovery cron: %w", err)
	}

	// Schedule oracle checks every 30 seconds if oracles are configured.
	if s.oracleChecker != nil && len(s.oracleChecker.Networks()) > 0 {
		_, err = s.cron.AddFunc("*/30 * * * * *", func() {
			ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
			defer cancel()
			s.oracleChecker.CheckAll(ctx, s.timeout)
		})
		if err != nil {
			return fmt.Errorf("multi-chain scheduler: failed to add oracle check cron: %w", err)
		}
		s.logger.Info("Oracle checker scheduled",
			zap.Strings("networks", s.oracleChecker.Networks()),
		)
	}

	// Schedule archival checks every 30 seconds for networks with archival config.
	if s.archivalStore != nil && s.hasArchivalNetworks() {
		_, err = s.cron.AddFunc("*/30 * * * * *", func() {
			s.checkArchivalStatus()
		})
		if err != nil {
			return fmt.Errorf("multi-chain scheduler: failed to add archival check cron: %w", err)
		}
		s.logger.Info("Archival checker scheduled")
	}

	s.cron.Start()
	s.logger.Info("Multi-chain scheduler started",
		zap.Duration("health_check_timeout", s.timeout),
	)

	return nil
}

// Stop halts the scheduler and closes resources.
func (s *MultiChainScheduler) Stop() {
	s.logger.Info("Stopping multi-chain scheduler...")
	ctx := s.cron.Stop()
	<-ctx.Done()
	s.extChecker.Close()
	s.logger.Info("Multi-chain scheduler stopped")
}

// checkInternalNodes runs height and health checks for all internal nodes
// using the adapter engine and V2 config types directly.
func (s *MultiChainScheduler) checkInternalNodes() {
	cfg := s.configLoader.Get()
	s.timeout = cfg.Timeouts.HealthCheck

	for _, node := range cfg.Internals {
		node := node

		network := cfg.FindNetwork(node.Network)
		if network == nil {
			s.logger.Warn("Node references unknown network",
				zap.String("node", node.Name),
				zap.String("network", node.Network),
			)
			continue
		}

		// Get the adapter for this network type.
		adpt, err := s.registry.Get(network.Type)
		if err != nil {
			s.logger.Warn("No adapter for network type",
				zap.String("network", node.Network),
				zap.String("type", network.Type),
				zap.Error(err),
			)
			continue
		}

		// Build adapter configs from V2 types.
		netCfg := V2NetworkToAdapterConfig(network)

		// Run height check.
		checkCfg, err := adpt.HeightCheck(netCfg)
		if err != nil {
			s.logger.Warn("Failed to get height check config",
				zap.String("network", node.Network),
				zap.Error(err),
			)
			continue
		}

		// Determine the node URL for the height check protocol.
		nodeURL := node.Endpoints[checkCfg.Protocol]
		if nodeURL == "" {
			s.logger.Debug("Node has no endpoint for height check protocol",
				zap.String("node", node.Name),
				zap.String("protocol", checkCfg.Protocol),
			)
			continue
		}

		// Skip gRPC height checks — engine only handles HTTP.
		if !checkCfg.IsGRPC {
			_ = s.pool.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
				defer cancel()

				start := time.Now()
				height, err := s.engine.CheckHeight(ctx, checkCfg, urlutil.NormalizeURL(nodeURL))
				latency := time.Since(start)

				if err != nil {
					recordCheckError(s.logger, node.Network, node.Name, checkCfg.Protocol, "engine", err)
					metrics.NodeAvailable.WithLabelValues(node.Network, node.Name, checkCfg.Protocol).Set(0)
					s.healthStore.SetUnhealthy(node.Network, node.Name, checkCfg.Protocol, err.Error())
					return
				}

				recordCheckSuccess(s.heightStore, s.cache, s.logger, ctx, node.Network, node.Name, checkCfg.Protocol, height, latency)
				s.healthStore.SetHealthy(node.Network, node.Name, checkCfg.Protocol)
			})
		}

		// Run health checks for other protocols.
		nodeCfg := v2NodeToAdapterConfig(node)
		healthChecks, err := adpt.HealthChecks(netCfg, nodeCfg)
		if err != nil {
			s.logger.Warn("Failed to get health check configs",
				zap.String("node", node.Name),
				zap.Error(err),
			)
			continue
		}

		for _, hc := range healthChecks {
			hc := hc

			// Skip health check for the protocol that already does height check.
			if hc.Protocol == checkCfg.Protocol {
				continue
			}

			hcNodeURL := node.Endpoints[hc.Protocol]
			if hcNodeURL == "" {
				continue
			}

			if hc.IsGRPC {
				s.scheduleGRPCHealthCheck(node, hcNodeURL, hc.Protocol)
			} else if hc.IsWebSocket {
				s.scheduleWebSocketHealthCheck(node, hcNodeURL, hc.Protocol)
			} else {
				s.scheduleHTTPHealthCheck(node, hcNodeURL, hc)
			}
		}
	}
}

// scheduleHTTPHealthCheck runs an HTTP health check via the adapter engine.
func (s *MultiChainScheduler) scheduleHTTPHealthCheck(node config.MultiChainNode, nodeURL string, hc adapter.HealthCheckConfig) {
	_ = s.pool.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		defer cancel()

		err := s.engine.CheckHealth(ctx, hc, urlutil.NormalizeURL(nodeURL))
		if err != nil {
			s.healthStore.SetUnhealthy(node.Network, node.Name, hc.Protocol, err.Error())
			s.logger.Debug("Health check failed",
				zap.String("node", node.Name),
				zap.String("protocol", hc.Protocol),
				zap.Error(err),
			)
			return
		}
		s.healthStore.SetHealthy(node.Network, node.Name, hc.Protocol)
	})
}

// scheduleGRPCHealthCheck verifies that a gRPC endpoint is reachable by
// dialing and waiting for the connection to reach the READY state.
func (s *MultiChainScheduler) scheduleGRPCHealthCheck(node config.MultiChainNode, grpcAddr string, protocol string) {
	_ = s.pool.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		defer cancel()

		cfg := s.configLoader.Get()
		ka := keepalive.ClientParameters{
			Time:                cfg.GRPCKeepalive.Time,
			Timeout:             cfg.GRPCKeepalive.Timeout,
			PermitWithoutStream: cfg.GRPCKeepalive.PermitWithoutStream,
		}

		conn, err := grpcutil.NewConnection(grpcAddr, node.GRPCInsecure, ka)
		if err != nil {
			s.healthStore.SetUnhealthy(node.Network, node.Name, protocol, err.Error())
			s.logger.Debug("gRPC health check: dial failed",
				zap.String("node", node.Name),
				zap.String("addr", grpcAddr),
				zap.Error(err),
			)
			return
		}
		defer func() { _ = conn.Close() }()

		// Wait for the connection to reach READY state.
		// gRPC connections transition: IDLE → CONNECTING → READY (or TRANSIENT_FAILURE).
		// We poll state changes until we reach a terminal outcome or the context expires.
		conn.Connect()
		for {
			state := conn.GetState()
			if state == connectivity.Ready {
				s.healthStore.SetHealthy(node.Network, node.Name, protocol)
				return
			}
			if state == connectivity.TransientFailure || state == connectivity.Shutdown {
				errMsg := fmt.Sprintf("gRPC connection state: %s", state)
				s.healthStore.SetUnhealthy(node.Network, node.Name, protocol, errMsg)
				s.logger.Debug("gRPC health check: not ready",
					zap.String("node", node.Name),
					zap.String("state", state.String()),
				)
				return
			}
			// Still transitioning (IDLE or CONNECTING) — wait for next state change.
			if !conn.WaitForStateChange(ctx, state) {
				// Context expired before reaching READY.
				s.healthStore.SetUnhealthy(node.Network, node.Name, protocol, "gRPC dial timeout: did not reach READY")
				s.logger.Debug("gRPC health check: timeout",
					zap.String("node", node.Name),
					zap.String("addr", grpcAddr),
				)
				return
			}
		}
	})
}

// scheduleWebSocketHealthCheck verifies that a WebSocket endpoint is reachable
// using the existing CheckWebSocket function.
func (s *MultiChainScheduler) scheduleWebSocketHealthCheck(node config.MultiChainNode, wsURL string, protocol string) {
	_ = s.pool.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		defer cancel()

		// CheckWebSocket expects an HTTP URL and converts to WS internally.
		// For nodes that have a separate websocket endpoint, the URL is the RPC URL.
		// Find the RPC URL if this is a websocket check on a Cosmos node.
		checkURL := wsURL
		if rpcURL, ok := node.Endpoints["rpc"]; ok && protocol == "websocket" {
			checkURL = rpcURL
		}

		ok := CheckWebSocket(ctx, checkURL, s.logger)
		if ok {
			s.healthStore.SetHealthy(node.Network, node.Name, protocol)
		} else {
			s.healthStore.SetUnhealthy(node.Network, node.Name, protocol, "websocket check failed")
		}
	})
}

// checkExternalRings delegates to the existing ExternalChecker.
func (s *MultiChainScheduler) checkExternalRings() {
	cfg := s.configLoader.Get()
	s.timeout = cfg.Timeouts.HealthCheck

	networks := s.getAllNetworks(cfg)

	for _, external := range cfg.Externals {
		external := external

		for _, network := range networks {
			network := network

			_ = s.pool.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
				defer cancel()

				if err := s.extChecker.CheckExternal(ctx, external, network); err != nil {
					s.logger.Debug("External check failed",
						zap.String("external", external.Name),
						zap.String("network", network),
						zap.Error(err),
					)
				}
			})
		}
	}
}

func (s *MultiChainScheduler) getAllNetworks(cfg *config.MultiChainConfig) []string {
	networksMap := make(map[string]bool)
	for _, node := range cfg.Internals {
		networksMap[node.Network] = true
	}
	for _, network := range cfg.Networks {
		networksMap[network.Name] = true
	}

	networks := make([]string, 0, len(networksMap))
	for network := range networksMap {
		networks = append(networks, network)
	}
	return networks
}

func (s *MultiChainScheduler) recoverFailedEndpoints() {
	cfg := s.configLoader.Get()
	s.timeout = cfg.Timeouts.HealthCheck

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	s.extChecker.RecoverFailedEndpoints(ctx)
	s.extChecker.UpdateEndpointMetrics()
}

// hasArchivalNetworks returns true if any network has archival config.
func (s *MultiChainScheduler) hasArchivalNetworks() bool {
	cfg := s.configLoader.Get()
	for _, net := range cfg.Networks {
		if net.Archival != nil {
			return true
		}
	}
	return false
}

// checkArchivalStatus checks if nodes have historical block data at the configured min_height.
// For EVM chains: uses eth_getBlockByNumber(min_height).
// For Cosmos chains: uses /blocks/{min_height} on REST.
// For custom: uses the same height check config with the min_height substituted.
func (s *MultiChainScheduler) checkArchivalStatus() {
	cfg := s.configLoader.Get()

	for _, network := range cfg.Networks {
		if network.Archival == nil {
			continue
		}

		minHeight := network.Archival.MinHeight
		checkCfg := s.buildArchivalCheckConfig(&network, minHeight)
		if checkCfg == nil {
			continue
		}

		for _, node := range cfg.Internals {
			if node.Network != network.Name {
				continue
			}

			// Get the node URL for the archival check protocol.
			nodeURL := node.Endpoints[checkCfg.Protocol]
			if nodeURL == "" {
				continue
			}

			node := node
			checkCfg := *checkCfg
			_ = s.pool.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
				defer cancel()

				height, err := s.engine.CheckHeight(ctx, checkCfg, urlutil.NormalizeURL(nodeURL))
				if err != nil || height <= 0 {
					s.archivalStore.SetNotArchival(network.Name, node.Name)
					s.logger.Info("Node is not archival",
						zap.String("node", node.Name),
						zap.String("network", network.Name),
						zap.Int64("min_height", minHeight),
						zap.Error(err),
					)
					return
				}

				s.archivalStore.SetArchival(network.Name, node.Name)
				s.logger.Info("Node confirmed archival",
					zap.String("node", node.Name),
					zap.String("network", network.Name),
					zap.Int64("min_height", minHeight),
					zap.Int64("returned_height", height),
				)
			})
		}
	}
}

// buildArchivalCheckConfig uses the adapter interface to produce the archival check config.
func (s *MultiChainScheduler) buildArchivalCheckConfig(network *config.MultiChainNetwork, minHeight int64) *adapter.CheckConfig {
	adpt, err := s.registry.Get(network.Type)
	if err != nil {
		s.logger.Warn("No adapter for archival check",
			zap.String("network", network.Name),
			zap.String("type", network.Type),
			zap.Error(err),
		)
		return nil
	}

	netCfg := V2NetworkToAdapterConfig(network)
	cfg, err := adpt.ArchivalCheck(netCfg, minHeight)
	if err != nil {
		s.logger.Warn("Failed to build archival check config",
			zap.String("network", network.Name),
			zap.Error(err),
		)
		return nil
	}
	return &cfg
}

// V2NetworkToAdapterConfig converts a V2 MultiChainNetwork to the adapter's NetworkConfig.
func V2NetworkToAdapterConfig(net *config.MultiChainNetwork) adapter.NetworkConfig {
	cfg := adapter.NetworkConfig{
		Name: net.Name,
		Type: net.Type,
		Mode: net.Mode,
	}

	// Map endpoints.
	for _, ep := range net.Endpoints {
		cfg.Endpoints = append(cfg.Endpoints, adapter.EndpointConfig{
			Protocol: ep.Protocol,
			Listen:   ep.Listen,
		})
	}

	// Map height check overrides for custom adapter type.
	if net.HeightCheck != nil {
		cfg.HeightCheck = &adapter.HeightCheckOverride{
			Protocol:       net.HeightCheck.Protocol,
			Method:         net.HeightCheck.Method,
			URLPath:        net.HeightCheck.URLPath,
			Headers:        net.HeightCheck.Headers,
			Body:           net.HeightCheck.Body,
			ResponsePath:   net.HeightCheck.ResponsePath,
			ResponseFormat: net.HeightCheck.ResponseFormat,
		}
	}

	return cfg
}

// v2NodeToAdapterConfig converts a V2 MultiChainNode to the adapter's NodeConfig.
func v2NodeToAdapterConfig(node config.MultiChainNode) adapter.NodeConfig {
	return adapter.NodeConfig{
		Name:     node.Name,
		Network:  node.Network,
		Endpoint: node.Endpoints,
	}
}

// NewMultiChainHTTPClient creates the shared HTTP client for the multi-chain engine.
// Used by server/ when wiring the multi-chain scheduler.
func NewMultiChainHTTPClient() *http.Client {
	return newCheckerHTTPClient()
}
