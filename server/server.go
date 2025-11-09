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
	configLoader  *config.Loader
	logger        *zap.Logger
	pool          pond.Pool
	scheduler     *checker.Scheduler
	store         *storage.HeightStore
	cache         *storage.Cache
	endpointStore *storage.ExternalEndpointStore
	selector      *selector.Selector
	statusServer  *http.Server
	httpServers   []*http.Server // All HTTP proxy servers (API + RPC)
	grpcServers   []*grpc.Server // All gRPC proxy servers
}

// New creates a new Sauron server
func New(configPath string) (*Server, error) {
	// Initialize logger
	logger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("failed to create logger: %w", err)
	}

	logger.Info("The Eye of Sauron awakens...", zap.String("config", configPath))

	// Load configuration
	configLoader, err := config.NewLoader(configPath, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	cfg := configLoader.Get()

	// Initialize storage
	store := storage.NewHeightStore()
	logger.Info("The Dark Lord's memory initialized")

	// Initialize external endpoint store
	endpointStore := storage.NewExternalEndpointStore(logger)
	logger.Info("External endpoint tracking initialized")

	// Initialize cache (optional)
	var cacheURI string
	if cfg.Redis.Enabled {
		cacheURI = cfg.Redis.URI
	}
	cache := storage.NewCache(cacheURI, logger)

	// Initialize worker pool (The servants of Sauron)
	ctx := context.Background()
	pool := pond.NewPool(100, pond.WithContext(ctx))
	logger.Info("Worker pool created", zap.Int("workers", 100))

	// Initialize selector
	sel := selector.NewSelector(store, endpointStore, configLoader, logger)
	logger.Info("The Dark Lord's judgment ready")

	// Initialize scheduler
	sched := checker.NewScheduler(store, cache, endpointStore, configLoader, pool, logger)

	return &Server{
		configLoader:  configLoader,
		logger:        logger,
		pool:          pool,
		scheduler:     sched,
		store:         store,
		cache:         cache,
		endpointStore: endpointStore,
		selector:      sel,
	}, nil
}

// Start begins all Sauron services
func (s *Server) Start() error {
	cfg := s.configLoader.Get()

	// Start scheduler (The Eye never sleeps)
	if err := s.scheduler.Start(); err != nil {
		return fmt.Errorf("failed to start scheduler: %w", err)
	}

	// Start status server (The Palantír)
	if err := s.startStatusServer(cfg); err != nil {
		return err
	}

	// Start proxy servers (The gates) - one set per network
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
func (s *Server) startStatusServer(cfg *config.Config) error {
	mux := http.NewServeMux()

	// Setup status routes
	handler := status.NewHandler(s.selector, s.configLoader, s.logger)
	handler.SetupRoutes(mux)

	s.statusServer = &http.Server{
		Addr:    cfg.Listen,
		Handler: mux,
	}

	go func() {
		s.logger.Info("Status server starting", zap.String("addr", cfg.Listen))
		if err := s.statusServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Fatal("Status server failed", zap.Error(err))
		}
	}()

	return nil
}

// startNetworkProxies starts proxy servers for each configured network
func (s *Server) startNetworkProxies(cfg *config.Config) error {
	for _, network := range cfg.Networks {
		// Start API proxy for this network
		if cfg.API && network.APIListen != "" {
			proxyHandler := proxy.NewHTTPProxy(s.selector, s.configLoader, s.endpointStore, s.logger, "api", network.Name)
			server := &http.Server{
				Addr:    network.APIListen,
				Handler: proxyHandler,
			}
			s.httpServers = append(s.httpServers, server)

			go func(netName, addr string) {
				s.logger.Info("API proxy starting",
					zap.String("network", netName),
					zap.String("addr", addr),
				)
				if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					s.logger.Fatal("API proxy failed", zap.String("network", netName), zap.Error(err))
				}
			}(network.Name, network.APIListen)
		}

		// Start RPC proxy for this network
		if cfg.RPC && network.RPCListen != "" {
			proxyHandler := proxy.NewHTTPProxy(s.selector, s.configLoader, s.endpointStore, s.logger, "rpc", network.Name)
			server := &http.Server{
				Addr:    network.RPCListen,
				Handler: proxyHandler,
			}
			s.httpServers = append(s.httpServers, server)

			go func(netName, addr string) {
				s.logger.Info("RPC proxy starting",
					zap.String("network", netName),
					zap.String("addr", addr),
				)
				if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					s.logger.Fatal("RPC proxy failed", zap.String("network", netName), zap.Error(err))
				}
			}(network.Name, network.RPCListen)
		}

		// Start gRPC proxy for this network
		if cfg.GRPC && network.GRPCListen != "" {
			grpcProxy := proxy.NewGRPCProxy(s.selector, s.configLoader, s.endpointStore, s.logger, network.Name)
			grpcServer := grpcProxy.GetServer()
			s.grpcServers = append(s.grpcServers, grpcServer)

			go func(netName, addr string) {
				s.logger.Info("gRPC proxy starting",
					zap.String("network", netName),
					zap.String("addr", addr),
				)

				// Create TCP listener
				lis, err := net.Listen("tcp", addr)
				if err != nil {
					s.logger.Fatal("gRPC proxy failed to listen",
						zap.String("network", netName),
						zap.Error(err))
				}

				if err := grpcServer.Serve(lis); err != nil {
					s.logger.Fatal("gRPC proxy failed",
						zap.String("network", netName),
						zap.Error(err))
				}
			}(network.Name, network.GRPCListen)
		}
	}

	return nil
}

// WaitForShutdown waits for shutdown signal and performs graceful shutdown
func (s *Server) WaitForShutdown() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	sig := <-sigCh
	s.logger.Info("Shutdown signal received", zap.String("signal", sig.String()))

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

	// Stop worker pool
	s.pool.StopAndWait()

	// Close cache
	if err := s.cache.Close(); err != nil {
		s.logger.Error("Cache close error", zap.Error(err))
	}

	s.logger.Info("Shutdown complete. The Eye closes.")
}
