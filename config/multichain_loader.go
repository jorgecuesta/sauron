package config

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sauron/metrics"

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
	reloadMu sync.Mutex                // serializes read+store so the newest file always wins
	onReload []func(*MultiChainConfig) // guarded by reloadMu
}

// OnReload registers fn to run after every reload that changes the running
// config (including one that keeps startup-only fields), with the config now
// in effect. Subscribers run in registration order, serialized with reloads,
// so they see configs in the order they were applied; they must not call
// Reload or OnReload.
func (l *MultiChainLoader) OnReload(fn func(*MultiChainConfig)) {
	l.reloadMu.Lock()
	defer l.reloadMu.Unlock()
	l.onReload = append(l.onReload, fn)
}

// ErrRestartRequired is returned by Reload when the new file changes fields the
// server only reads at startup. The rest of the file is applied; those fields
// keep their running values until a restart.
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
	metrics.ConfigReloadOK.Set(1)
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
	LogReloadResult(l.logger, "file change", l.Reload())
}

// Reload re-reads the config file and applies it if it is valid; on a parse or
// validation error the current config stays. Fields the server only reads at
// startup keep their running values: the rest is applied and Reload returns
// ErrRestartRequired naming them. Calls are serialized and each reads the file
// afresh through its own viper instance, so concurrent reloads (SIGHUP and the
// file watcher) end on the newest file.
func (l *MultiChainLoader) Reload() error {
	l.reloadMu.Lock()
	defer l.reloadMu.Unlock()

	err := l.reload()
	if err != nil {
		metrics.ConfigReloadOK.Set(0)
	} else {
		metrics.ConfigReloadOK.Set(1)
	}
	if err == nil || errors.Is(err, ErrRestartRequired) {
		cfg := l.Get()
		for _, fn := range l.onReload {
			fn(cfg)
		}
	}
	return err
}

func (l *MultiChainLoader) reload() error {
	v := newConfigViper(l.path)
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	cfg, err := parseMultiChain(v)
	if err != nil {
		return err
	}

	eff, kept := keepStartupOnly(l.Get(), cfg)
	if len(kept) == 0 {
		l.store(cfg)
		return nil
	}
	if err := ValidateMultiChain(eff); err != nil {
		return fmt.Errorf("invalid configuration after keeping startup-only fields (%s): %w", strings.Join(kept, ", "), err)
	}
	l.store(eff)
	return fmt.Errorf("%w: %s changed", ErrRestartRequired, strings.Join(kept, ", "))
}

// keepStartupOnly returns new with every field the running server only reads
// at startup reset to its value in old, plus the names of the fields that
// differed. Startup-only, with where they are read:
//   - listen (status http.Server), auth and rate_limit (status.NewHandler),
//     redis (storage.NewCache), timeouts.proxy (http.Transport in NewHTTPProxy),
//     grpc_keepalive (cached gRPC client connections);
//   - the set of networks and each one's type, endpoint listeners and gRPC
//     message sizes (listeners and servers built in server.New);
//   - whether a network has archival (ArchivalFilter, archival cron) and its
//     whole sync block (SyncFilter max drift, OracleChecker configs);
//   - mode and height_check (except interval, which the scheduler reads
//     every tick), frozen into default oracle checks;
//   - an internal node's grpc_insecure while its gRPC URL is unchanged (the
//     pooled connection is keyed by URL).
//
// Live: timeouts.health_check, endpoint advertise URLs, archival.min_height
// and check_interval, height_check.interval, circuit_breaker, internals (nodes,
// URLs), externals, users and external_failover_threshold.
func keepStartupOnly(old, new *MultiChainConfig) (*MultiChainConfig, []string) {
	eff := *new
	var kept []string

	if old.Listen != new.Listen {
		eff.Listen = old.Listen
		kept = append(kept, "listen")
	}
	if old.Auth != new.Auth {
		eff.Auth = old.Auth
		kept = append(kept, "auth")
	}
	if old.RateLimit != new.RateLimit {
		eff.RateLimit = old.RateLimit
		kept = append(kept, "rate_limit")
	}
	if old.Redis != new.Redis {
		eff.Redis = old.Redis
		kept = append(kept, "redis")
	}
	if old.Timeouts.Proxy != new.Timeouts.Proxy {
		eff.Timeouts.Proxy = old.Timeouts.Proxy
		kept = append(kept, "timeouts.proxy")
	}
	if old.GRPCKeepalive != new.GRPCKeepalive {
		eff.GRPCKeepalive = old.GRPCKeepalive
		kept = append(kept, "grpc_keepalive")
	}

	eff.Networks, kept = keepStartupNetworks(old.Networks, new.Networks, kept)
	eff.Internals, kept = keepStartupInternals(old.Internals, new.Internals, kept)
	return &eff, kept
}

// keepStartupNetworks keeps the running set of networks (matched by name, so
// reordering the YAML changes nothing) and, inside each, the startup-only
// fields, taking the live ones from the new file.
func keepStartupNetworks(old, new []MultiChainNetwork, kept []string) ([]MultiChainNetwork, []string) {
	newByName := make(map[string]MultiChainNetwork, len(new))
	for _, n := range new {
		newByName[n.Name] = n
	}
	setChanged := len(old) != len(new)
	for _, o := range old {
		if _, ok := newByName[o.Name]; !ok {
			setChanged = true
		}
	}
	if setChanged {
		kept = append(kept, "networks")
	}

	out := make([]MultiChainNetwork, 0, len(old))
	for _, o := range old {
		n, ok := newByName[o.Name]
		if !ok {
			out = append(out, o) // removed from the file, but its listener still serves
			continue
		}
		e := n
		prefix := "networks[" + o.Name + "]"
		if o.Type != n.Type {
			e.Type = o.Type
			kept = append(kept, prefix+".type")
		}
		var endpointsKept bool
		e.Endpoints, endpointsKept = keepStartupEndpoints(o.Endpoints, n.Endpoints)
		if endpointsKept {
			kept = append(kept, prefix+".endpoints")
		}
		if (o.Archival == nil) != (n.Archival == nil) {
			e.Archival = o.Archival
			kept = append(kept, prefix+".archival")
		}
		if !reflect.DeepEqual(o.Sync, n.Sync) {
			e.Sync = o.Sync
			kept = append(kept, prefix+".sync")
		}
		if o.Mode != n.Mode {
			e.Mode = o.Mode
			kept = append(kept, prefix+".mode")
		}
		if !sameHeightCheck(o.HeightCheck, n.HeightCheck) {
			e.HeightCheck = o.HeightCheck
			kept = append(kept, prefix+".height_check")
		}
		out = append(out, e)
	}
	return out, kept
}

// sameHeightCheck compares height checks ignoring interval, which nothing
// reads at runtime yet (the height cron is fixed), so changing it is neither
// applied nor a restart change.
func sameHeightCheck(a, b *HeightCheckConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	ac, bc := *a, *b
	ac.Interval, bc.Interval = 0, 0
	return reflect.DeepEqual(ac, bc)
}

// keepStartupEndpoints keeps the running endpoints (matched by protocol) and
// takes only their advertise URL, which is read per request, from new. It
// reports whether anything other than advertise differed.
func keepStartupEndpoints(old, new []NetworkEndpoint) ([]NetworkEndpoint, bool) {
	newByProto := make(map[string]NetworkEndpoint, len(new))
	for _, ep := range new {
		newByProto[ep.Protocol] = ep
	}
	differs := len(old) != len(new)
	out := make([]NetworkEndpoint, 0, len(old))
	for _, o := range old {
		e := o
		if n, ok := newByProto[o.Protocol]; ok {
			e.Advertise = n.Advertise
			n.Advertise = o.Advertise
			if n != o {
				differs = true
			}
		} else {
			differs = true
		}
		out = append(out, e)
	}
	return out, differs
}

// keepStartupInternals keeps an internal node's running grpc_insecure while
// its gRPC URL is unchanged: the pooled connection is keyed by URL and would
// keep its old TLS setting anyway.
func keepStartupInternals(old, new []MultiChainNode, kept []string) ([]MultiChainNode, []string) {
	oldByName := make(map[string]MultiChainNode, len(old))
	for _, n := range old {
		oldByName[n.Name] = n
	}
	out := make([]MultiChainNode, len(new))
	copy(out, new)
	for i, n := range out {
		o, ok := oldByName[n.Name]
		if ok && o.GRPCInsecure != n.GRPCInsecure && o.Endpoints["grpc"] == n.Endpoints["grpc"] {
			out[i].GRPCInsecure = o.GRPCInsecure
			kept = append(kept, "internals["+n.Name+"].grpc_insecure")
		}
	}
	return out, kept
}

// LogReloadResult logs the outcome of a Reload triggered by trigger (e.g.
// "SIGHUP", "file change"). ErrRestartRequired means the file was applied
// except for startup-only fields, so it is a warning, not a failure.
func LogReloadResult(logger *zap.Logger, trigger string, err error) {
	switch {
	case err == nil:
	case errors.Is(err, ErrRestartRequired):
		logger.Warn("Configuration applied except startup-only fields; restart to apply them",
			zap.String("trigger", trigger), zap.Error(err))
	default:
		logger.Error("Configuration reload failed; keeping previous configuration",
			zap.String("trigger", trigger), zap.Error(err))
	}
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
