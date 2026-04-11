package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sauron/adapter"
	"sauron/adapter/cosmos"
	"sauron/adapter/custom"
	"sauron/adapter/evm"
	"sauron/adapter/solana"
	"sauron/checker"
	"sauron/config"
	"sauron/proxy"
	"sauron/selector"
	"sauron/status"
	"sauron/storage"

	"github.com/alitto/pond/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// Server orchestrates all components of Sauron
// The foundation of Barad-dûr
type Server struct {
	configLoader   *config.MultiChainLoader
	logger         *zap.Logger
	pool           pond.Pool
	scheduler      *checker.MultiChainScheduler
	store          *storage.HeightStore
	healthStore    *storage.HealthStore   // per-node per-protocol health
	archivalStore  *storage.ArchivalStore // archival node tracking
	oracleStore    *storage.OracleStore   // oracle reference heights
	cache          *storage.Cache
	endpointStore  *storage.ExternalEndpointStore
	selector       *selector.Selector
	registry       *adapter.Registry      // adapter registry
	engine         *adapter.Engine        // check engine
	oracleChecker  *checker.OracleChecker // oracle height checker
	circuitBreaker *proxy.CircuitBreaker  // shared circuit breaker for HTTP proxies
	statusServer   *http.Server
	statusHandler  *status.Handler    // Kept so Shutdown() can call handler.Shutdown()
	httpServers    []*http.Server     // All HTTP proxy servers
	grpcServers    []*grpc.Server     // All gRPC proxy servers
	grpcProxies    []*proxy.GRPCProxy // All gRPC proxy instances (for Close)
	errCh          chan error         // Fatal errors from background goroutines
}

// New creates a new Sauron server
func New(configPath string) (*Server, error) {
	// Initialize logger
	logger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("failed to create logger: %w", err)
	}

	logger.Info("The Eye of Sauron awakens...", zap.String("config", configPath))

	// Load V2 configuration
	configLoader, err := config.NewMultiChainLoader(configPath, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	cfg := configLoader.Get()

	// Initialize storage
	store := storage.NewHeightStore()
	healthStore := storage.NewHealthStore()
	archivalStore := storage.NewArchivalStore()
	oracleStore := storage.NewOracleStore()
	logger.Info("The Dark Lord's memory initialized")

	// Initialize external endpoint store
	endpointStore := storage.NewExternalEndpointStore(logger)

	// Initialize cache (optional)
	var cacheURI string
	if cfg.Redis.Enabled {
		cacheURI = cfg.Redis.URI
	}
	cache := storage.NewCache(cacheURI, logger)

	// Initialize worker pool
	ctx := context.Background()
	pool := pond.NewPool(100, pond.WithContext(ctx))
	logger.Info("Worker pool created", zap.Int("workers", 100))

	// Initialize adapter registry with all built-in adapters.
	registry := adapter.NewRegistry()
	for _, factory := range []adapter.ChainAdapter{cosmos.New(), evm.New(), solana.New(), custom.New()} {
		if err := registry.Register(factory); err != nil {
			return nil, fmt.Errorf("failed to register %s adapter: %w", factory.Type(), err)
		}
	}

	engine := adapter.NewEngine(checker.NewMultiChainHTTPClient())

	logger.Info("Adapter engine initialized",
		zap.Strings("adapters", registry.Types()),
	)

	// Initialize selector with V2 config loader.
	sel := selector.NewSelector(store, endpointStore, configLoader, logger)
	sel.SetHealthStore(healthStore)

	// Initialize selector filters.
	archivalFilter := selector.NewArchivalFilter(archivalStore, logger)
	syncFilter := selector.NewSyncFilter(oracleStore, logger)
	sel.SetArchivalFilter(archivalFilter)
	sel.SetSyncFilter(syncFilter)

	// Initialize oracle checker.
	oracleChecker := checker.NewOracleChecker(engine, oracleStore, logger)

	// Configure filters and oracles from V2 network config.
	for _, network := range cfg.Networks {
		if network.Archival != nil {
			archivalFilter.RequireArchival(network.Name)
			logger.Info("Archival filter enabled",
				zap.String("network", network.Name),
				zap.Int64("min_height", network.Archival.MinHeight),
			)
		}

		if network.Sync != nil {
			syncFilter.SetMaxDrift(network.Name, network.Sync.MaxDrift)
			logger.Info("Sync filter enabled",
				zap.String("network", network.Name),
				zap.Int64("max_drift", network.Sync.MaxDrift),
			)

			// Build oracle configs from sync oracles.
			oracleCfg, err := buildOracleConfig(&network, registry)
			if err != nil {
				return nil, fmt.Errorf("network %s: failed to build oracle config: %w", network.Name, err)
			}
			{
				oracleChecker.AddConfig(oracleCfg)
				logger.Info("Oracle checker configured",
					zap.String("network", network.Name),
					zap.Int("oracles", len(oracleCfg.Oracles)),
				)
			}
		}
	}

	// Initialize scheduler with V2 config.
	sched := checker.NewMultiChainScheduler(engine, registry, store, healthStore, archivalStore, cache, endpointStore, oracleChecker, configLoader, pool, logger)

	// Initialize shared circuit breaker backed by the health store.
	cb := proxy.NewCircuitBreaker(healthStore, logger)

	logger.Info("The Dark Lord's judgment ready",
		zap.Int("networks", len(cfg.Networks)),
		zap.Int("internal_nodes", len(cfg.Internals)),
	)

	return &Server{
		configLoader:   configLoader,
		logger:         logger,
		pool:           pool,
		scheduler:      sched,
		store:          store,
		healthStore:    healthStore,
		archivalStore:  archivalStore,
		oracleStore:    oracleStore,
		cache:          cache,
		endpointStore:  endpointStore,
		selector:       sel,
		registry:       registry,
		engine:         engine,
		oracleChecker:  oracleChecker,
		circuitBreaker: cb,
		errCh:          make(chan error, 10),
	}, nil
}

// buildOracleConfig creates an OracleConfig for a network's sync configuration.
// For each oracle, it either uses the oracle's override config or falls back
// to the network's height check config via the adapter factory.
func buildOracleConfig(network *config.MultiChainNetwork, registry *adapter.Registry) (checker.OracleConfig, error) {
	oracleCfg := checker.OracleConfig{
		Network:  network.Name,
		Interval: network.Sync.CheckInterval,
	}

	adpt, err := registry.Get(network.Type)
	if err != nil {
		return oracleCfg, fmt.Errorf("no adapter for type %q: %w", network.Type, err)
	}

	// Get the network's default height check config for oracles without overrides.
	netAdapterCfg := checker.V2NetworkToAdapterConfig(network)
	defaultCheck, err := adpt.HeightCheck(netAdapterCfg)
	if err != nil {
		return oracleCfg, fmt.Errorf("failed to get default height check: %w", err)
	}

	for _, oracle := range network.Sync.Oracles {
		var checkCfg adapter.CheckConfig

		if oracle.ResponsePath != "" {
			// Oracle has custom config — build CheckConfig from overrides.
			checkCfg = adapter.CheckConfig{
				Method:         oracle.Method,
				URLPath:        oracle.URLPath,
				ResponsePath:   oracle.ResponsePath,
				ResponseFormat: oracle.ResponseFormat,
				Protocol:       "http",
			}
			if oracle.Headers != nil {
				for k, v := range oracle.Headers {
					if checkCfg.Headers == nil {
						checkCfg.Headers = make(map[string][]string)
					}
					checkCfg.Headers.Set(k, v)
				}
			}
			if oracle.Body != "" {
				checkCfg.Body = []byte(oracle.Body)
			}
		} else {
			// No override — reuse the network's height check config.
			checkCfg = defaultCheck
		}

		oracleCfg.Oracles = append(oracleCfg.Oracles, checker.OracleEndpoint{
			URL:   oracle.URL,
			Check: checkCfg,
		})
	}

	return oracleCfg, nil
}

// Start begins all Sauron services
func (s *Server) Start() error {
	cfg := s.configLoader.Get()

	// Start scheduler
	if err := s.scheduler.Start(); err != nil {
		return fmt.Errorf("failed to start scheduler: %w", err)
	}

	// Start status server
	if err := s.startStatusServer(cfg); err != nil {
		return err
	}

	// Start proxy servers — one set per network endpoint
	if err := s.startNetworkProxies(cfg); err != nil {
		return err
	}

	s.logger.Info("Sauron is fully operational - The tower stands",
		zap.String("status_listen", cfg.Listen),
		zap.Int("networks", len(cfg.Networks)),
	)

	return nil
}

// startStatusServer starts the status API server
func (s *Server) startStatusServer(cfg *config.MultiChainConfig) error {
	mux := http.NewServeMux()

	handler := status.NewHandler(s.selector, s.configLoader, s.logger)
	handler.SetupRoutes(mux)
	s.statusHandler = handler

	s.statusServer = &http.Server{
		Addr:    cfg.Listen,
		Handler: mux,
	}

	go func() {
		s.logger.Info("Status server starting", zap.String("addr", cfg.Listen))
		if err := s.statusServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("Status server failed", zap.Error(err))
			s.errCh <- err
		}
	}()

	return nil
}

// startNetworkProxies starts proxy servers for each configured network endpoint.
// V2: iterates network.Endpoints instead of checking global API/RPC/GRPC flags.
func (s *Server) startNetworkProxies(cfg *config.MultiChainConfig) error {
	for _, network := range cfg.Networks {
		for _, endpoint := range network.Endpoints {
			switch endpoint.Protocol {
			case "grpc":
				s.startGRPCProxy(network.Name, endpoint.Listen)
			default:
				// All non-gRPC protocols (rest, rpc, jsonrpc, http) use HTTP proxy.
				s.startHTTPProxy(network.Name, endpoint.Protocol, endpoint.Listen)
			}
		}
	}
	return nil
}

// startHTTPProxy starts an HTTP proxy for a network endpoint.
func (s *Server) startHTTPProxy(networkName, protocol, listenAddr string) {
	proxyHandler := proxy.NewHTTPProxy(s.selector, s.configLoader, s.endpointStore, s.logger, protocol, networkName, s.circuitBreaker)
	server := &http.Server{
		Addr:    listenAddr,
		Handler: proxyHandler,
	}
	s.httpServers = append(s.httpServers, server)

	go func() {
		s.logger.Info("HTTP proxy starting",
			zap.String("network", networkName),
			zap.String("protocol", protocol),
			zap.String("addr", listenAddr),
		)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("HTTP proxy failed",
				zap.String("network", networkName),
				zap.String("protocol", protocol),
				zap.Error(err),
			)
			s.errCh <- err
		}
	}()
}

// startGRPCProxy starts a gRPC proxy for a network endpoint.
func (s *Server) startGRPCProxy(networkName, listenAddr string) {
	grpcProxy := proxy.NewGRPCProxy(s.selector, s.configLoader, s.endpointStore, s.logger, networkName, s.circuitBreaker)
	grpcServer := grpcProxy.GetServer()
	s.grpcServers = append(s.grpcServers, grpcServer)
	s.grpcProxies = append(s.grpcProxies, grpcProxy)

	go func() {
		s.logger.Info("gRPC proxy starting",
			zap.String("network", networkName),
			zap.String("addr", listenAddr),
		)

		lis, err := net.Listen("tcp", listenAddr)
		if err != nil {
			s.logger.Error("gRPC proxy failed to listen",
				zap.String("network", networkName),
				zap.Error(err))
			s.errCh <- err
			return
		}

		if err := grpcServer.Serve(lis); err != nil {
			s.logger.Error("gRPC proxy failed",
				zap.String("network", networkName),
				zap.Error(err))
			s.errCh <- err
		}
	}()
}

// WaitForShutdown waits for a shutdown signal or a fatal error from a background
// goroutine, then performs graceful shutdown.
func (s *Server) WaitForShutdown() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		s.logger.Info("Shutdown signal received", zap.String("signal", sig.String()))
	case err := <-s.errCh:
		s.logger.Error("Fatal error in background goroutine, initiating shutdown", zap.Error(err))
	}

	s.Shutdown()
}

// Shutdown performs graceful shutdown
func (s *Server) Shutdown() {
	s.logger.Info("The Dark Tower falls... performing graceful shutdown")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Stop scheduler
	s.scheduler.Stop()

	// Stop status server
	if s.statusServer != nil {
		if err := s.statusServer.Shutdown(ctx); err != nil {
			s.logger.Error("Status server shutdown error", zap.Error(err))
		}
	}

	// Stop all HTTP proxy servers
	for i, httpServer := range s.httpServers {
		if err := httpServer.Shutdown(ctx); err != nil {
			s.logger.Error("HTTP proxy server shutdown error",
				zap.Int("server_index", i),
				zap.String("addr", httpServer.Addr),
				zap.Error(err))
		} else {
			s.logger.Info("HTTP proxy server shutdown successfully",
				zap.String("addr", httpServer.Addr))
		}
	}

	// Stop all gRPC proxy servers
	for i, grpcServer := range s.grpcServers {
		grpcServer.GracefulStop()
		s.logger.Info("gRPC proxy server shutdown successfully",
			zap.Int("server_index", i))
	}

	// Close all gRPC proxy backend connection pools
	for i, grpcProxy := range s.grpcProxies {
		if err := grpcProxy.Close(); err != nil {
			s.logger.Error("gRPC proxy close error",
				zap.Int("proxy_index", i),
				zap.Error(err))
		}
	}

	// Stop the status handler's rate limiter goroutine
	if s.statusHandler != nil {
		s.statusHandler.Shutdown()
	}

	// Stop worker pool
	s.pool.StopAndWait()

	// Close cache
	if err := s.cache.Close(); err != nil {
		s.logger.Error("Cache close error", zap.Error(err))
	}

	s.logger.Info("Shutdown complete. The Eye closes.")
}
