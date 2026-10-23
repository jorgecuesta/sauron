package checker

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"sauron/adapter"
	"sauron/config"
	"sauron/internal/grpcutil"
	"sauron/internal/urlutil"
	"sauron/metrics"
	"sauron/storage"

	"github.com/alitto/pond/v2"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
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
	timeout       atomic.Int64                  // health-check timeout (ns); written by every cron job, so atomic
	lastRun       *xsync.Map[string, time.Time] // when each periodic check last ran, by "kind:network"
	inFlight      sync.Map                      // check key -> *atomic.Bool, set while that check runs
}

// goOnce runs fn on the pool unless the previous run of the check named key is
// still going. With an interval shorter than a check's timeout, a slow node
// would otherwise get overlapping checks that finish out of order.
func (s *MultiChainScheduler) goOnce(key string, fn func()) {
	v, _ := s.inFlight.LoadOrStore(key, new(atomic.Bool))
	running := v.(*atomic.Bool)
	if !running.CompareAndSwap(false, true) {
		return
	}
	if err := s.pool.Go(func() {
		defer running.Store(false)
		fn()
	}); err != nil {
		running.Store(false)
	}
}

func newLastRunMap() *xsync.Map[string, time.Time] {
	return xsync.NewMap[string, time.Time]()
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

	s := &MultiChainScheduler{
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
		lastRun:       newLastRunMap(),
	}
	s.timeout.Store(int64(5 * time.Second))
	return s
}

// checkTimeout returns the current health-check timeout.
func (s *MultiChainScheduler) checkTimeout() time.Duration {
	return time.Duration(s.timeout.Load())
}

// Start begins scheduled checks. A one-second tick runs each network's
// internal node, archival and oracle checks when their configured interval has
// elapsed (read from the current config, so a reload applies at once); the
// external ring checks and their recovery use fixed intervals.
func (s *MultiChainScheduler) Start() error {
	cfg := s.configLoader.Get()
	s.timeout.Store(int64(cfg.Timeouts.HealthCheck))

	_, err := s.cron.AddFunc("* * * * * *", func() {
		s.tick(time.Now())
	})
	if err != nil {
		return fmt.Errorf("multi-chain scheduler: failed to add check tick: %w", err)
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

	if s.oracleChecker != nil && len(s.oracleChecker.Networks()) > 0 {
		s.logger.Info("Oracle checker scheduled",
			zap.Strings("networks", s.oracleChecker.Networks()),
		)
	}
	if s.archivalStore != nil && s.hasArchivalNetworks() {
		s.logger.Info("Archival checker scheduled")
	}

	s.cron.Start()
	s.logger.Info("Multi-chain scheduler started",
		zap.Duration("health_check_timeout", s.checkTimeout()),
	)

	return nil
}

// CloseExternalConnectionsExcept closes the external checker's cached gRPC
// connections whose URL keep rejects; see ExternalChecker.CloseConnectionsExcept.
func (s *MultiChainScheduler) CloseExternalConnectionsExcept(keep func(url string) bool) int {
	return s.extChecker.CloseConnectionsExcept(keep)
}

// Stop halts the scheduler and closes resources.
func (s *MultiChainScheduler) Stop() {
	s.logger.Info("Stopping multi-chain scheduler...")
	ctx := s.cron.Stop()
	<-ctx.Done()
	s.extChecker.Close()
	s.logger.Info("Multi-chain scheduler stopped")
}

// due reports whether the check named key should run at now, given its
// interval, and if so records now as its last run. It is atomic per key, so
// overlapping ticks never both run a check; a clock that goes back never runs
// one early.
func (s *MultiChainScheduler) due(key string, interval time.Duration, now time.Time) bool {
	ran := false
	s.lastRun.Compute(key, func(last time.Time, loaded bool) (time.Time, xsync.ComputeOp) {
		if loaded && now.Sub(last) < interval {
			return last, xsync.CancelOp
		}
		ran = true
		return now, xsync.UpdateOp
	})
	return ran
}

// tick runs every periodic check whose interval has elapsed at now.
func (s *MultiChainScheduler) tick(now time.Time) {
	cfg := s.configLoader.Get()
	s.timeout.Store(int64(cfg.Timeouts.HealthCheck))

	for i := range cfg.Networks {
		network := &cfg.Networks[i]
		if s.due("height:"+network.Name, network.HeightCheckInterval(), now) {
			s.checkNetworkNodes(cfg, network.Name)
		}
		if s.archivalStore != nil && network.Archival != nil && s.due("archival:"+network.Name, network.Archival.Interval(), now) {
			s.checkArchivalNetwork(cfg, network)
		}
	}

	if s.oracleChecker != nil && len(s.oracleChecker.Networks()) > 0 {
		_ = s.pool.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), s.checkTimeout())
			defer cancel()
			s.oracleChecker.CheckDue(ctx, s.checkTimeout(), func(network string, interval time.Duration) bool {
				return s.due("oracle:"+network, interval, now)
			})
		})
	}
}

// checkInternalNodes runs height and health checks for all internal nodes
// using the adapter engine and V2 config types directly.
func (s *MultiChainScheduler) checkInternalNodes() {
	cfg := s.configLoader.Get()
	s.timeout.Store(int64(cfg.Timeouts.HealthCheck))
	s.checkNetworkNodes(cfg, "")
}

// checkNetworkNodes runs height and health checks for the internal nodes of
// networkName, or of every network when it is empty.
func (s *MultiChainScheduler) checkNetworkNodes(cfg *config.MultiChainConfig, networkName string) {
	for _, node := range cfg.Internals {
		node := node
		if networkName != "" && node.Network != networkName {
			continue
		}

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
			s.goOnce("height:"+node.Network+":"+node.Name, func() {
				ctx, cancel := context.WithTimeout(context.Background(), s.checkTimeout())
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
			if hcNodeURL == "" && hc.IsWebSocket {
				// CometBFT serves WebSocket on the RPC port (/websocket), so a node
				// without an explicit websocket endpoint is checked through its RPC URL.
				hcNodeURL = node.Endpoints["rpc"]
			}
			if hcNodeURL == "" {
				continue
			}

			if hc.IsGRPC {
				s.scheduleGRPCHealthCheck(node, hcNodeURL, hc.Protocol)
			} else if hc.IsWebSocket {
				s.scheduleWebSocketHealthCheck(node, hcNodeURL, hc.Protocol, checkCfg.Protocol)
			} else {
				s.scheduleHTTPHealthCheck(node, hcNodeURL, hc)
			}
		}
	}
}

// scheduleHTTPHealthCheck runs an HTTP health check via the adapter engine.
func (s *MultiChainScheduler) scheduleHTTPHealthCheck(node config.MultiChainNode, nodeURL string, hc adapter.HealthCheckConfig) {
	s.goOnce("health:"+node.Network+":"+node.Name+":"+hc.Protocol, func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.checkTimeout())
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

// grpcHealthQueryMethod is the query the gRPC health check sends once the
// transport is READY. Its request is empty and its answer is not decoded, so
// it needs no proto types.
const grpcHealthQueryMethod = "/cosmos.base.tendermint.v1beta1.Service/GetLatestBlock"

// grpcHealthQuery marks the node by a real query: a READY transport says
// nothing about whether the node answers. A node-failure status (the ones the
// proxy's circuit breaker counts) or no answer within the check timeout is
// unhealthy; any other answer, such as Unimplemented on a non-Cosmos chain,
// proves the node serves.
func (s *MultiChainScheduler) grpcHealthQuery(conn *grpc.ClientConn, node config.MultiChainNode, protocol string) {
	// The query gets the whole check timeout of its own: reaching READY may
	// have used most of the dial's, and that is no reason to call the node
	// unhealthy.
	ctx, cancel := context.WithTimeout(context.Background(), s.checkTimeout())
	defer cancel()
	// The answer is not decoded: only whether and how the node answered
	// matters, and a local decoding error must not count as a node failure.
	req, resp := []byte{}, []byte{}
	err := conn.Invoke(ctx, grpcHealthQueryMethod, &req, &resp, grpc.ForceCodec(grpcutil.BytesCodec{}))
	code := status.Code(err)
	if grpcutil.IsNodeFailure(code) || code == codes.DeadlineExceeded {
		s.healthStore.SetUnhealthy(node.Network, node.Name, protocol, "gRPC health query: "+err.Error())
		s.logger.Debug("gRPC health check: query failed",
			zap.String("node", node.Name),
			zap.String("code", code.String()),
		)
		return
	}
	s.healthStore.SetHealthy(node.Network, node.Name, protocol)
}

// scheduleGRPCHealthCheck verifies that a gRPC endpoint is reachable by
// dialing and waiting for the connection to reach the READY state.
func (s *MultiChainScheduler) scheduleGRPCHealthCheck(node config.MultiChainNode, grpcAddr string, protocol string) {
	s.goOnce("health:"+node.Network+":"+node.Name+":"+protocol, func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.checkTimeout())
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
				s.grpcHealthQuery(conn, node, protocol)
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
//
// The result is also recorded as WebSocketAvailable on the node's height-store
// entry under heightProtocol: that entry is what the selector returns and what
// the HTTP proxy checks before allowing a WebSocket upgrade.
func (s *MultiChainScheduler) scheduleWebSocketHealthCheck(node config.MultiChainNode, wsURL, protocol, heightProtocol string) {
	s.goOnce("health:"+node.Network+":"+node.Name+":"+protocol, func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.checkTimeout())
		defer cancel()

		// CheckWebSocket expects an HTTP URL and converts to WS internally.
		// For nodes that have a separate websocket endpoint, the URL is the RPC URL.
		// Find the RPC URL if this is a websocket check on a Cosmos node.
		checkURL := wsURL
		if rpcURL, ok := node.Endpoints["rpc"]; ok && protocol == "websocket" {
			checkURL = rpcURL
		}

		ok := CheckWebSocket(ctx, checkURL, s.logger)
		s.heightStore.UpdateWebSocketAvailability(node.Network, node.Name, heightProtocol, ok)
		if ok {
			s.healthStore.SetHealthy(node.Network, node.Name, protocol)
			metrics.NodeWebSocketAvailable.WithLabelValues(node.Network, node.Name, protocol).Set(1)
		} else {
			s.healthStore.SetUnhealthy(node.Network, node.Name, protocol, "websocket check failed")
			metrics.NodeWebSocketAvailable.WithLabelValues(node.Network, node.Name, protocol).Set(0)
		}
	})
}

// checkExternalRings delegates to the existing ExternalChecker.
func (s *MultiChainScheduler) checkExternalRings() {
	cfg := s.configLoader.Get()
	s.timeout.Store(int64(cfg.Timeouts.HealthCheck))

	networks := s.getAllNetworks(cfg)

	for _, external := range cfg.Externals {
		external := external

		for _, network := range networks {
			network := network

			_ = s.pool.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), s.checkTimeout())
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
	s.timeout.Store(int64(cfg.Timeouts.HealthCheck))

	ctx, cancel := context.WithTimeout(context.Background(), s.checkTimeout())
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

	for i := range cfg.Networks {
		if cfg.Networks[i].Archival != nil {
			s.checkArchivalNetwork(cfg, &cfg.Networks[i])
		}
	}
}

// checkArchivalNetwork checks whether each internal node of network serves
// its archival min_height.
func (s *MultiChainScheduler) checkArchivalNetwork(cfg *config.MultiChainConfig, network *config.MultiChainNetwork) {
	minHeight := network.Archival.MinHeight
	checkCfg := s.buildArchivalCheckConfig(network, minHeight)
	if checkCfg == nil {
		return
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
			ctx, cancel := context.WithTimeout(context.Background(), s.checkTimeout())
			defer cancel()

			height, err := s.engine.CheckHeight(ctx, checkCfg, urlutil.NormalizeURL(nodeURL))
			isArchival := err == nil && height > 0
			// Info only when the status changes; the same answer every
			// check_interval is Debug.
			logAt := s.logger.Debug
			if prev := s.archivalStore.GetStatus(network.Name, node.Name); prev == nil || prev.IsArchival != isArchival {
				logAt = s.logger.Info
			}
			if !isArchival {
				s.archivalStore.SetNotArchival(network.Name, node.Name)
				logAt("Node is not archival",
					zap.String("node", node.Name),
					zap.String("network", network.Name),
					zap.Int64("min_height", minHeight),
					zap.Error(err),
				)
				return
			}

			s.archivalStore.SetArchival(network.Name, node.Name)
			logAt("Node confirmed archival",
				zap.String("node", node.Name),
				zap.String("network", network.Name),
				zap.Int64("min_height", minHeight),
				zap.Int64("returned_height", height),
			)
		})
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
