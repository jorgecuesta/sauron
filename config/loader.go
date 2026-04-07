package config

import (
	"fmt"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// Loader handles configuration loading and hot reloading
// The keeper of the ancient texts
type Loader struct {
	config atomic.Pointer[Config]
	logger *zap.Logger
	v      *viper.Viper
}

// NewLoader creates a new configuration loader
func NewLoader(configPath string, logger *zap.Logger) (*Loader, error) {
	l := &Loader{
		logger: logger,
		v:      viper.New(),
	}

	// Configure Viper
	l.v.SetConfigFile(configPath)
	l.v.SetConfigType("yaml")

	// Load initial configuration
	if err := l.v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	// Unmarshal into struct
	var cfg Config
	if err := l.v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Validate configuration
	if err := Validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	l.config.Store(&cfg)
	logger.Info("Configuration loaded successfully",
		zap.String("path", configPath),
		zap.Int("internal_nodes", len(cfg.Internals)),
		zap.Int("external_rings", len(cfg.Externals)),
		zap.Int("users", len(cfg.Users)),
	)

	// Set up hot reload
	l.v.WatchConfig()
	l.v.OnConfigChange(l.onConfigChange)

	return l, nil
}

// onConfigChange handles configuration file changes
func (l *Loader) onConfigChange(e fsnotify.Event) {
	l.logger.Info("Configuration file changed, reloading...", zap.String("event", e.String()))

	var newCfg Config
	if err := l.v.Unmarshal(&newCfg); err != nil {
		l.logger.Error("Failed to unmarshal new config", zap.Error(err))
		return
	}

	if err := Validate(&newCfg); err != nil {
		l.logger.Error("Invalid new configuration", zap.Error(err))
		return
	}

	l.config.Store(&newCfg)

	l.logger.Info("Configuration reloaded successfully",
		zap.Int("internal_nodes", len(newCfg.Internals)),
		zap.Int("external_rings", len(newCfg.Externals)),
		zap.Int("users", len(newCfg.Users)),
	)
}

// Get returns the current configuration (thread-safe, zero allocation).
// The returned *Config is immutable — callers must not modify it.
func (l *Loader) Get() *Config {
	return l.config.Load()
}
