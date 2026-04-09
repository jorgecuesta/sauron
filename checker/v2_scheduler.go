package checker

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"sauron/adapter"
	"sauron/config"
	"sauron/internal/urlutil"
	"sauron/metrics"
	"sauron/storage"

	"github.com/alitto/pond/v2"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
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
	configLoader  *config.Loader
	logger        *zap.Logger
	timeout       time.Duration
}

// NewMultiChainScheduler creates a scheduler with adapter-driven checks.
func NewMultiChainScheduler(
	engine *adapter.Engine,
	registry *adapter.Registry,
	heightStore *storage.HeightStore,
	healthStore *storage.HealthStore,
	cache *storage.Cache,
	endpointStore *storage.ExternalEndpointStore,
	configLoader *config.Loader,
	pool pond.Pool,
	logger *zap.Logger,
) *MultiChainScheduler {
	// External checker still uses v1 code — external ring protocol is unchanged.
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

// checkInternalNodes runs height checks for all internal nodes using the adapter engine.
func (s *MultiChainScheduler) checkInternalNodes() {
	cfg := s.configLoader.Get()
	s.timeout = cfg.Timeouts.HealthCheck

	for _, node := range cfg.Internals {
		node := node

		// Find the network config to get the adapter type.
		network := cfg.FindNetwork(node.Network)
		if network == nil {
			s.logger.Warn("Node references unknown network",
				zap.String("node", node.Name),
				zap.String("network", node.Network),
			)
			continue
		}

		// Get the adapter for this network type.
		// For v1 config compatibility, cosmos is the only supported type.
		adpt, err := s.registry.Get("cosmos")
		if err != nil {
			s.logger.Warn("No adapter for network type",
				zap.String("network", node.Network),
				zap.Error(err),
			)
			continue
		}

		// Build adapter NetworkConfig from v1 config.
		netCfg := v1NetworkToAdapterConfig(network)

		// Get height check config.
		checkCfg, err := adpt.HeightCheck(netCfg)
		if err != nil {
			s.logger.Warn("Failed to get height check config",
				zap.String("network", node.Network),
				zap.Error(err),
			)
			continue
		}

		// Determine the node URL for the height check protocol.
		nodeURL := v1NodeURL(node, checkCfg.Protocol)
		if nodeURL == "" {
			continue
		}

		// Skip gRPC height checks — they use the existing GRPCChecker path.
		if checkCfg.IsGRPC {
			continue
		}

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

		// Run health checks for other protocols.
		nodeCfg := v1NodeToAdapterConfig(node)
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

			// Skip gRPC and WebSocket health checks — not yet implemented in engine.
			if hc.IsGRPC || hc.IsWebSocket {
				continue
			}

			hcNodeURL := v1NodeURL(node, hc.Protocol)
			if hcNodeURL == "" {
				continue
			}

			_ = s.pool.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
				defer cancel()

				err := s.engine.CheckHealth(ctx, hc, urlutil.NormalizeURL(hcNodeURL))
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
	}
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

func (s *MultiChainScheduler) getAllNetworks(cfg *config.Config) []string {
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

// v1NetworkToAdapterConfig converts a v1 Network config to the adapter's NetworkConfig.
func v1NetworkToAdapterConfig(net *config.Network) adapter.NetworkConfig {
	return adapter.NetworkConfig{
		Name: net.Name,
		Type: "cosmos", // v1 only supports cosmos
	}
}

// v1NodeToAdapterConfig converts a v1 Node config to the adapter's NodeConfig.
func v1NodeToAdapterConfig(node config.Node) adapter.NodeConfig {
	endpoints := make(map[string]string)
	if node.API != "" {
		endpoints["rest"] = node.API
	}
	if node.RPC != "" {
		endpoints["rpc"] = node.RPC
	}
	if node.GRPC != "" {
		endpoints["grpc"] = node.GRPC
	}
	return adapter.NodeConfig{
		Name:     node.Name,
		Network:  node.Network,
		Endpoint: endpoints,
	}
}

// v1NodeURL returns the URL for a given protocol from a v1 Node config.
func v1NodeURL(node config.Node, protocol string) string {
	switch protocol {
	case "rest":
		return node.API
	case "rpc":
		return node.RPC
	case "grpc":
		return node.GRPC
	default:
		return ""
	}
}

// NewMultiChainHTTPClient creates the shared HTTP client for the multi-chain engine.
// Used by server/ when wiring the multi-chain scheduler.
func NewMultiChainHTTPClient() *http.Client {
	return newCheckerHTTPClient()
}
