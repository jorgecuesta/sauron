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
		v:      newConfigViper(configPath),
	}

	if err := l.v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	cfg, err := parseMultiChain(l.v)
	if err != nil {
		return nil, err
	}

	l.config.Store(cfg)
	logger.Info("Multi-chain configuration loaded",
		zap.String("path", configPath),
		zap.Int("networks", len(cfg.Networks)),
		zap.Int("internal_nodes", len(cfg.Internals)),
		zap.Int("external_rings", len(cfg.Externals)),
		zap.Int("users", len(cfg.Users)),
	)

	// Register the callback before starting the watcher: the watcher goroutine
	// reads it, so setting it afterwards is a data race.
	l.v.OnConfigChange(l.onConfigChange)
	l.v.WatchConfig()

	return l, nil
}

// newConfigViper returns a viper instance bound to the YAML file at path.
func newConfigViper(path string) *viper.Viper {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")
	return v
}

// parseMultiChain decodes, validates and defaults the config held by v. Every
// load path goes through it so they all decode the same way.
func parseMultiChain(v *viper.Viper) (*MultiChainConfig, error) {
	var cfg MultiChainConfig
	if err := v.Unmarshal(&cfg, viper.DecodeHook(
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
	return &cfg, nil
}

func (l *MultiChainLoader) onConfigChange(e fsnotify.Event) {
	l.logger.Info("Configuration file changed, reloading...", zap.String("event", e.String()))

	cfg, err := parseMultiChain(l.v)
	if err != nil {
		l.logger.Error("Failed to reload changed config; keeping previous configuration", zap.Error(err))
		return
	}
	l.store(cfg)
}

// Reload re-reads the config file and swaps it in if it is valid; on any error
// the current config stays. It reads through its own viper instance, so it is
// safe alongside the file watcher and other Reload calls.
func (l *MultiChainLoader) Reload() error {
	v := newConfigViper(l.v.ConfigFileUsed())
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	cfg, err := parseMultiChain(v)
	if err != nil {
		return err
	}
	l.store(cfg)
	return nil
}

// store swaps in cfg and logs the reload.
func (l *MultiChainLoader) store(cfg *MultiChainConfig) {
	l.config.Store(cfg)
	l.logger.Info("Configuration reloaded successfully",
		zap.Int("networks", len(cfg.Networks)),
		zap.Int("internal_nodes", len(cfg.Internals)),
		zap.Int("external_rings", len(cfg.Externals)),
		zap.Int("users", len(cfg.Users)),
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
