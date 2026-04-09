package config

import (
	"fmt"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// MultiChainLoader handles loading and hot reloading of MultiChainConfig.
type MultiChainLoader struct {
	config atomic.Pointer[MultiChainConfig]
	logger *zap.Logger
	v      *viper.Viper
}

// NewMultiChainLoader creates a new multi-chain configuration loader.
func NewMultiChainLoader(configPath string, logger *zap.Logger) (*MultiChainLoader, error) {
	l := &MultiChainLoader{
		logger: logger,
		v:      viper.New(),
	}

	l.v.SetConfigFile(configPath)
	l.v.SetConfigType("yaml")

	if err := l.v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var cfg MultiChainConfig
	if err := l.v.Unmarshal(&cfg, viper.DecodeHook(
		mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
			permissionsDecodeHook(),
		),
	)); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	if err := ValidateMultiChain(&cfg); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	applyMultiChainDefaults(&cfg)

	l.config.Store(&cfg)
	logger.Info("Multi-chain configuration loaded",
		zap.String("path", configPath),
		zap.Int("networks", len(cfg.Networks)),
		zap.Int("internal_nodes", len(cfg.Internals)),
		zap.Int("external_rings", len(cfg.Externals)),
		zap.Int("users", len(cfg.Users)),
	)

	l.v.WatchConfig()
	l.v.OnConfigChange(l.onConfigChange)

	return l, nil
}

func (l *MultiChainLoader) onConfigChange(e fsnotify.Event) {
	l.logger.Info("Configuration file changed, reloading...", zap.String("event", e.String()))

	var newCfg MultiChainConfig
	if err := l.v.Unmarshal(&newCfg, viper.DecodeHook(permissionsDecodeHook())); err != nil {
		l.logger.Error("Failed to unmarshal new config", zap.Error(err))
		return
	}

	if err := ValidateMultiChain(&newCfg); err != nil {
		l.logger.Error("Invalid new configuration", zap.Error(err))
		return
	}

	applyMultiChainDefaults(&newCfg)

	l.config.Store(&newCfg)

	l.logger.Info("Configuration reloaded successfully",
		zap.Int("networks", len(newCfg.Networks)),
		zap.Int("internal_nodes", len(newCfg.Internals)),
		zap.Int("external_rings", len(newCfg.Externals)),
		zap.Int("users", len(newCfg.Users)),
	)
}

// Get returns the current configuration (thread-safe, zero allocation).
// The returned *MultiChainConfig is immutable — callers must not modify it.
func (l *MultiChainLoader) Get() *MultiChainConfig {
	return l.config.Load()
}

// applyMultiChainDefaults sets safe defaults for zero-valued fields.
func applyMultiChainDefaults(cfg *MultiChainConfig) {
	if cfg.GRPCKeepalive.Time == 0 {
		cfg.GRPCKeepalive.Time = 600 * time.Second
	}
	if cfg.GRPCKeepalive.Timeout == 0 {
		cfg.GRPCKeepalive.Timeout = 20 * time.Second
	}
	if cfg.ExternalFailoverThreshold == 0 {
		cfg.ExternalFailoverThreshold = 2
	}

	// Set default height check intervals per network.
	for i := range cfg.Networks {
		if cfg.Networks[i].HeightCheck != nil && cfg.Networks[i].HeightCheck.Interval == 0 {
			cfg.Networks[i].HeightCheck.Interval = 30 * time.Second
		}
	}
}

// permissionsDecodeHook handles the "permissions: all" string shorthand
// and the map form for MultiChainPermissions.
func permissionsDecodeHook() mapstructure.DecodeHookFuncType {
	return func(from reflect.Type, to reflect.Type, data any) (any, error) {
		if to != reflect.TypeOf(MultiChainPermissions{}) {
			return data, nil
		}

		switch v := data.(type) {
		case string:
			if v == "all" {
				return MultiChainPermissions{All: true}, nil
			}
			return nil, fmt.Errorf("permissions: unknown string value %q (expected 'all' or a map)", v)
		case map[string]any:
			// Convert map[string]any → map[string]map[string]bool
			networks := make(map[string]map[string]bool)
			for netName, protos := range v {
				protoMap, ok := protos.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("permissions.%s: expected map of protocols, got %T", netName, protos)
				}
				networks[netName] = make(map[string]bool)
				for proto, allowed := range protoMap {
					b, ok := allowed.(bool)
					if !ok {
						return nil, fmt.Errorf("permissions.%s.%s: expected bool, got %T", netName, proto, allowed)
					}
					networks[netName][proto] = b
				}
			}
			return MultiChainPermissions{Networks: networks}, nil
		default:
			return data, nil
		}
	}
}
