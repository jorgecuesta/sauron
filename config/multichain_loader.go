package config

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// MultiChainLoader handles loading and hot reloading of MultiChainConfig.
type MultiChainLoader struct {
	config   atomic.Pointer[MultiChainConfig]
	logger   *zap.Logger
	v        *viper.Viper
	path     string
	reloadMu sync.Mutex // serializes read+store so the newest file always wins
}

// ErrRestartRequired is returned by Reload when the new file changes a field
// the server only reads at startup; the running config is kept.
var ErrRestartRequired = errors.New("configuration change requires a restart")

// NewMultiChainLoader creates a new multi-chain configuration loader.
func NewMultiChainLoader(configPath string, logger *zap.Logger) (*MultiChainLoader, error) {
	l := &MultiChainLoader{
		logger: logger,
		v:      newConfigViper(configPath),
		path:   configPath,
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

	// Re-read the file rather than use what viper read when the event fired: it
	// may have changed again since, and a SIGHUP reload may already hold it.
	if err := l.Reload(); err != nil {
		l.logger.Error("Failed to reload changed config; keeping previous configuration", zap.Error(err))
	}
}

// Reload re-reads the config file and swaps it in if it is valid and changes
// nothing that needs a restart; on any error the current config stays. Calls
// are serialized and each reads the file afresh through its own viper
// instance, so concurrent reloads (SIGHUP and the file watcher) end on the
// newest file.
func (l *MultiChainLoader) Reload() error {
	l.reloadMu.Lock()
	defer l.reloadMu.Unlock()

	v := newConfigViper(l.path)
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	cfg, err := parseMultiChain(v)
	if err != nil {
		return err
	}
	if field := restartOnlyChange(l.Get(), cfg); field != "" {
		return fmt.Errorf("%w: %s changed", ErrRestartRequired, field)
	}
	l.store(cfg)
	return nil
}

// restartOnlyChange names the first field that differs between old and new
// among those the server reads only at startup: the status listener, the auth
// middleware wiring and rate limiter (status.NewHandler), the Redis cache and
// the per-network proxy listeners (server.New). It returns "" if none differ.
func restartOnlyChange(old, new *MultiChainConfig) string {
	switch {
	case old.Listen != new.Listen:
		return "listen"
	case old.Auth != new.Auth:
		return "auth"
	case old.RateLimit != new.RateLimit:
		return "rate_limit"
	case old.Redis != new.Redis:
		return "redis"
	case len(old.Networks) != len(new.Networks):
		return "networks"
	}
	for i := range old.Networks {
		o, n := old.Networks[i], new.Networks[i]
		if o.Name != n.Name || o.Type != n.Type || len(o.Endpoints) != len(n.Endpoints) {
			return "networks"
		}
		for j := range o.Endpoints {
			oe, ne := o.Endpoints[j], n.Endpoints[j]
			// Advertise is read per request, so it may change at runtime.
			oe.Advertise, ne.Advertise = "", ""
			if oe != ne {
				return "networks[" + o.Name + "].endpoints"
			}
		}
	}
	return ""
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
