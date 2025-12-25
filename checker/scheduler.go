package checker

import (
	"context"
	"time"

	"sauron/config"
	"sauron/storage"

	"github.com/alitto/pond/v2"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// Scheduler coordinates periodic height checks
// The Eye that never sleeps
type Scheduler struct {
	cron         *cron.Cron
	pool         pond.Pool
	apiChecker   *APIChecker
	rpcChecker   *RPCChecker
	grpcChecker  *GRPCChecker
	extChecker   *ExternalChecker
	configLoader *config.Loader
	logger       *zap.Logger
	timeout      time.Duration
}

// NewScheduler creates a new scheduler
func NewScheduler(
	store *storage.HeightStore,
	cache *storage.Cache,
	endpointStore *storage.ExternalEndpointStore,
	configLoader *config.Loader,
	pool pond.Pool,
	logger *zap.Logger,
) *Scheduler {
	// Create checkers
	apiChecker := NewAPIChecker(store, cache, logger)
	rpcChecker := NewRPCChecker(store, cache, logger)
	grpcChecker := NewGRPCChecker(store, cache, logger)
	extChecker := NewExternalChecker(store, endpointStore, logger)

	// Create cron with seconds support and panic recovery
	cronScheduler := cron.New(
		cron.WithSeconds(),
		cron.WithChain(
			cron.Recover(cron.DefaultLogger),
		),
	)

	s := &Scheduler{
		cron:         cronScheduler,
		pool:         pool,
		apiChecker:   apiChecker,
		rpcChecker:   rpcChecker,
		grpcChecker:  grpcChecker,
		extChecker:   extChecker,
		configLoader: configLoader,
		logger:       logger,
		timeout:      5 * time.Second, // Default, will be updated from config
	}

	return s
}

// Start begins the scheduled height checks
func (s *Scheduler) Start() error {
	cfg := s.configLoader.Get()
	s.timeout = cfg.Timeouts.HealthCheck

	// Schedule internal node checks every 30 seconds (aligned with block time)
	_, err := s.cron.AddFunc("*/30 * * * * *", func() {
		s.checkInternalNodes()
	})
	if err != nil {
		return err
	}

	// Schedule external ring checks every 10 seconds
	_, err = s.cron.AddFunc("*/10 * * * * *", func() {
		s.checkExternalRings()
	})
	if err != nil {
		return err
	}

	// Schedule health check recovery for failed endpoints every 10 seconds
	_, err = s.cron.AddFunc("*/10 * * * * *", func() {
		s.recoverFailedEndpoints()
	})
	if err != nil {
		return err
	}

	s.cron.Start()
	s.logger.Info("Scheduler started - The Eye never sleeps",
		zap.Duration("health_check_timeout", s.timeout),
	)

	return nil
}

// Stop halts the scheduler
func (s *Scheduler) Stop() {
	s.logger.Info("Stopping scheduler...")
	ctx := s.cron.Stop()
	<-ctx.Done()

	// Close gRPC connections
	if err := s.grpcChecker.Close(); err != nil {
		s.logger.Warn("Error closing gRPC connections", zap.Error(err))
	}

	// Close HTTP transports
	s.apiChecker.Close()
	s.rpcChecker.Close()
	s.extChecker.Close()

	s.logger.Info("Scheduler stopped")
}

// checkInternalNodes checks all internal nodes
func (s *Scheduler) checkInternalNodes() {
	cfg := s.configLoader.Get()
	s.timeout = cfg.Timeouts.HealthCheck // Update timeout in case config changed

	for _, node := range cfg.Internals {
		node := node // Capture for goroutine

		// Check API if enabled and configured
		if cfg.API && node.API != "" {
			_ = s.pool.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
				defer cancel()

				if err := s.apiChecker.CheckNode(ctx, node); err != nil {
					s.logger.Debug("API check failed",
						zap.String("node", node.Name),
						zap.Error(err),
					)
				}
			})
		}

		// Check RPC if enabled and configured
		if cfg.RPC && node.RPC != "" {
			_ = s.pool.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
				defer cancel()

				if err := s.rpcChecker.CheckNode(ctx, node); err != nil {
					s.logger.Debug("RPC check failed",
						zap.String("node", node.Name),
						zap.Error(err),
					)
				}
			})
		}

		// Check gRPC if enabled and configured
		if cfg.GRPC && node.GRPC != "" {
			_ = s.pool.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
				defer cancel()

				// Find the network config for this node to get grpc_insecure setting
				grpcInsecure := false
				for _, network := range cfg.Networks {
					if network.Name == node.Network {
						grpcInsecure = network.GRPCInsecure
						break
					}
				}

				if err := s.grpcChecker.CheckNode(ctx, node, grpcInsecure); err != nil {
					s.logger.Debug("gRPC check failed",
						zap.String("node", node.Name),
						zap.Error(err),
					)
				}
			})
		}
	}
}

// checkExternalRings queries all external Sauron rings
func (s *Scheduler) checkExternalRings() {
	cfg := s.configLoader.Get()
	s.timeout = cfg.Timeouts.HealthCheck

	// Get all networks being monitored
	networks := s.getAllNetworks(cfg)

	for _, external := range cfg.Externals {
		external := external // Capture for goroutine

		// Query each network
		for _, network := range networks {
			network := network // Capture for goroutine

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

// getAllNetworks returns a list of all networks from internal nodes and config.Networks
func (s *Scheduler) getAllNetworks(cfg *config.Config) []string {
	networksMap := make(map[string]bool)

	// Get networks from internal nodes
	for _, node := range cfg.Internals {
		networksMap[node.Network] = true
	}

	// Also get networks from the main config (so externals-only configs work)
	for _, network := range cfg.Networks {
		networksMap[network.Name] = true
	}

	networks := make([]string, 0, len(networksMap))
	for network := range networksMap {
		networks = append(networks, network)
	}
	return networks
}

// recoverFailedEndpoints attempts to recover failed external endpoints
func (s *Scheduler) recoverFailedEndpoints() {
	cfg := s.configLoader.Get()
	s.timeout = cfg.Timeouts.HealthCheck

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	s.extChecker.RecoverFailedEndpoints(ctx)

	// Also update aggregate metrics (leveraging the same 10-second schedule)
	s.extChecker.UpdateEndpointMetrics()
}
