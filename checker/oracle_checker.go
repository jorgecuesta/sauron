package checker

import (
	"context"
	"time"

	"sauron/adapter"
	"sauron/storage"

	"go.uber.org/zap"
)

// OracleConfig describes how to check oracle heights for a network.
type OracleConfig struct {
	Network  string
	Interval time.Duration
	Oracles  []OracleEndpoint
}

// OracleEndpoint describes a single oracle to check.
type OracleEndpoint struct {
	URL   string
	Check adapter.CheckConfig
}

// OracleChecker periodically queries external oracle sources to determine
// the canonical chain height for each network. Results are stored in
// OracleStore and consumed by the SyncFilter.
type OracleChecker struct {
	engine      *adapter.Engine
	oracleStore *storage.OracleStore
	configs     []OracleConfig
	logger      *zap.Logger
}

// NewOracleChecker creates a new oracle checker.
func NewOracleChecker(
	engine *adapter.Engine,
	oracleStore *storage.OracleStore,
	logger *zap.Logger,
) *OracleChecker {
	return &OracleChecker{
		engine:      engine,
		oracleStore: oracleStore,
		logger:      logger,
	}
}

// AddConfig registers oracle check configuration for a network.
func (c *OracleChecker) AddConfig(cfg OracleConfig) {
	c.configs = append(c.configs, cfg)
}

// CheckAll runs oracle height checks for all configured networks.
// Each oracle is queried and the highest height across all oracles
// for a network is stored.
func (c *OracleChecker) CheckAll(ctx context.Context, timeout time.Duration) {
	for _, cfg := range c.configs {
		for _, oracle := range cfg.Oracles {
			oracle := oracle
			network := cfg.Network

			checkCtx, cancel := context.WithTimeout(ctx, timeout)

			height, err := c.engine.CheckHeight(checkCtx, oracle.Check, oracle.URL)
			cancel()

			if err != nil {
				c.logger.Warn("Oracle check failed",
					zap.String("network", network),
					zap.String("oracle", oracle.URL),
					zap.Error(err),
				)
				continue
			}

			if height <= 0 {
				c.logger.Warn("Oracle returned non-positive height",
					zap.String("network", network),
					zap.String("oracle", oracle.URL),
					zap.Int64("height", height),
				)
				continue
			}

			c.oracleStore.Update(network, height, oracle.URL)

			c.logger.Debug("Oracle check successful",
				zap.String("network", network),
				zap.String("oracle", oracle.URL),
				zap.Int64("height", height),
			)
		}
	}
}

// Networks returns the list of networks with oracle configs.
func (c *OracleChecker) Networks() []string {
	networks := make([]string, 0, len(c.configs))
	for _, cfg := range c.configs {
		networks = append(networks, cfg.Network)
	}
	return networks
}
