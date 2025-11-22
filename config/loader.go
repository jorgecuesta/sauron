package config

import (
	"fmt"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// Loader handles configuration loading and hot reloading
// The keeper of the ancient texts
type Loader struct {
	config *Config
	mu     sync.RWMutex
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

	l.config = &cfg
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

	l.mu.Lock()
	l.config = &newCfg
	l.mu.Unlock()

	l.logger.Info("Configuration reloaded successfully",
		zap.Int("internal_nodes", len(newCfg.Internals)),
		zap.Int("external_rings", len(newCfg.Externals)),
		zap.Int("users", len(newCfg.Users)),
	)
}

// Get returns the current configuration (thread-safe)
func (l *Loader) Get() *Config {
	l.mu.RLock()
	defer l.mu.RUnlock()

	// Deep copy to prevent external modifications to slices
	cfg := Config{
		API:                       l.config.API,
		RPC:                       l.config.RPC,
		GRPC:                      l.config.GRPC,
		Auth:                      l.config.Auth,
		Listen:                    l.config.Listen,
		ExternalFailoverThreshold: l.config.ExternalFailoverThreshold,
		Timeouts:                  l.config.Timeouts,
		Redis:                     l.config.Redis,
		RateLimit:                 l.config.RateLimit,
		// Deep copy slices
		Networks:  make([]Network, len(l.config.Networks)),
		Internals: make([]Node, len(l.config.Internals)),
		Externals: make([]External, len(l.config.Externals)),
		Users:     make([]User, len(l.config.Users)),
	}

	// Copy slice elements
	copy(cfg.Networks, l.config.Networks)
	copy(cfg.Internals, l.config.Internals)
	copy(cfg.Externals, l.config.Externals)
	copy(cfg.Users, l.config.Users)

	// Deep copy nested slices in Externals (Rings field)
	for i := range cfg.Externals {
		cfg.Externals[i].Rings = make([]string, len(l.config.Externals[i].Rings))
		copy(cfg.Externals[i].Rings, l.config.Externals[i].Rings)
	}

	return &cfg
}
