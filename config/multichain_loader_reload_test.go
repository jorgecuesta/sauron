package config

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"sauron/metrics"

	"github.com/fsnotify/fsnotify"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
)

// reloadedYAML differs from validMultiChainYAML in fields that need every
// decode hook: durations, a new node and the "permissions: all" shorthand.
const reloadedYAML = `
listen: ":3000"
auth: false

timeouts:
  health_check: 7s
  proxy: 45s

networks:
  - name: pocket
    type: cosmos
    height_check:
      protocol: rpc
      interval: 10s
    endpoints:
      - protocol: rpc
        listen: ":8081"
      - protocol: rest
        listen: ":8080"

internals:
  - name: seed-one
    network: pocket
    endpoints:
      rpc: "http://seed-one:26657"
      rest: "http://seed-one:1317"
  - name: seed-two
    network: pocket
    endpoints:
      rpc: "http://seed-two:26657"
      rest: "http://seed-two:1317"

users:
  - name: ring-a
    token: "tok-a"
    permissions: all
`

func rewriteConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
}

// assertReloaded checks every field reloadedYAML changed.
func assertReloaded(t *testing.T, cfg *MultiChainConfig) {
	t.Helper()
	if cfg.Timeouts.HealthCheck != 7*time.Second {
		t.Errorf("Timeouts.HealthCheck = %s, want 7s", cfg.Timeouts.HealthCheck)
	}
	if cfg.Timeouts.Proxy != 45*time.Second {
		t.Errorf("Timeouts.Proxy = %s, want 45s", cfg.Timeouts.Proxy)
	}
	if got := cfg.Networks[0].HeightCheck.Interval; got != 10*time.Second {
		t.Errorf("HeightCheck.Interval = %s, want 10s", got)
	}
	if len(cfg.Internals) != 2 || cfg.Internals[1].Name != "seed-two" {
		t.Errorf("Internals = %+v, want seed-one and seed-two", cfg.Internals)
	}
	if len(cfg.Users) != 1 || !cfg.Users[0].Permissions.All {
		t.Errorf("Users = %+v, want one user with permissions all", cfg.Users)
	}
	if cfg.GRPCKeepalive.Time != 600*time.Second {
		t.Errorf("defaults not applied: GRPCKeepalive.Time = %s", cfg.GRPCKeepalive.Time)
	}
}

func TestMultiChainLoader_Reload_AppliesFileChanges(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t, validMultiChainYAML)
	loader, err := NewMultiChainLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("NewMultiChainLoader: %v", err)
	}

	rewriteConfig(t, path, reloadedYAML)
	if err := loader.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	assertReloaded(t, loader.Get())
}

func TestMultiChainLoader_Reload_RejectsBadFileKeepsPrevious(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{name: "malformed yaml", content: "networks: [\n  - name: pocket\n  bad"},
		{name: "fails validation", content: validMultiChainYAML + `
  - name: orphan
    network: does-not-exist
    endpoints:
      rpc: "http://orphan:26657"
`},
		{name: "bad duration", content: `
listen: ":3000"
timeouts:
  health_check: soon
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: rpc
        listen: ":8081"
internals:
  - name: seed-one
    network: pocket
    endpoints:
      rpc: "http://seed-one:26657"
`},
		{name: "empty file", content: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeTestConfig(t, validMultiChainYAML)
			loader, err := NewMultiChainLoader(path, zap.NewNop())
			if err != nil {
				t.Fatalf("NewMultiChainLoader: %v", err)
			}

			rewriteConfig(t, path, tc.content)
			if err := loader.Reload(); err == nil {
				t.Fatal("Reload: want error, got nil")
			}
			// Compared by content, not pointer: the file watcher is live and may
			// re-store the original valid file at any time.
			cfg := loader.Get()
			if cfg.Timeouts.HealthCheck != 5*time.Second {
				t.Errorf("Timeouts.HealthCheck = %s, want previous 5s", cfg.Timeouts.HealthCheck)
			}
			if len(cfg.Internals) != 1 || cfg.Internals[0].Name != "seed-one" {
				t.Errorf("Internals = %+v, want previous [seed-one]", cfg.Internals)
			}
		})
	}
}

// TestMultiChainLoader_WatcherReloadDecodesDurations: the file watcher must
// decode a changed file exactly like the initial load. It used to pass only the
// permissions hook to viper, which replaces viper's default duration hook, so
// any config with "5s"-style durations failed to reload.
func TestMultiChainLoader_WatcherReloadDecodesDurations(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t, validMultiChainYAML)
	loader, err := NewMultiChainLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("NewMultiChainLoader: %v", err)
	}

	rewriteConfig(t, path, reloadedYAML)

	deadline := time.Now().Add(5 * time.Second)
	for loader.Get().Timeouts.HealthCheck != 7*time.Second {
		if time.Now().After(deadline) {
			t.Fatalf("watcher never applied the change: HealthCheck = %s, want 7s", loader.Get().Timeouts.HealthCheck)
		}
		time.Sleep(50 * time.Millisecond)
	}
	assertReloaded(t, loader.Get())
}

// TestMultiChainLoader_Reload_Concurrent races Reload calls against readers.
func TestMultiChainLoader_Reload_Concurrent(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t, reloadedYAML)
	loader, err := NewMultiChainLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("NewMultiChainLoader: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := loader.Reload(); err != nil {
					t.Errorf("Reload: %v", err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if cfg := loader.Get(); cfg == nil || len(cfg.Internals) != 2 {
					t.Errorf("Get returned %+v during reload", cfg)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// newUnwatchedLoader builds a loader for path without starting the file
// watcher, so a test can drive onConfigChange itself, deterministically.
func newUnwatchedLoader(t *testing.T, path string) *MultiChainLoader {
	t.Helper()
	l := &MultiChainLoader{logger: zap.NewNop(), v: newConfigViper(path), path: path}
	if err := l.v.ReadInConfig(); err != nil {
		t.Fatalf("ReadInConfig: %v", err)
	}
	cfg, err := parseMultiChain(l.v)
	if err != nil {
		t.Fatalf("parseMultiChain: %v", err)
	}
	l.config.Store(cfg)
	return l
}

func withHealthCheck(yaml, d string) string {
	return strings.Replace(yaml, "health_check: 7s", "health_check: "+d, 1)
}

// TestMultiChainLoader_LateWatcherEventDoesNotRestoreOlderFile: the watcher's
// viper read the file at v2, then the file moved to v3 and SIGHUP reloaded v3.
// When the watcher's callback finally runs it must not put v2 back.
func TestMultiChainLoader_LateWatcherEventDoesNotRestoreOlderFile(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t, validMultiChainYAML)
	l := newUnwatchedLoader(t, path)

	rewriteConfig(t, path, withHealthCheck(reloadedYAML, "7s")) // v2
	if err := l.v.ReadInConfig(); err != nil {                  // what the watcher saw
		t.Fatalf("ReadInConfig v2: %v", err)
	}
	rewriteConfig(t, path, withHealthCheck(reloadedYAML, "9s")) // v3
	if err := l.Reload(); err != nil {
		t.Fatalf("Reload v3: %v", err)
	}

	l.onConfigChange(fsnotify.Event{Name: path, Op: fsnotify.Write}) // the late v2 event

	if got := l.Get().Timeouts.HealthCheck; got != 9*time.Second {
		t.Errorf("HealthCheck = %s, want 9s from the newest file", got)
	}
}

// TestMultiChainLoader_Reload_RejectsRestartOnlyChanges: fields the server
// only reads at startup (listeners, auth wiring, rate limiter, redis) cannot
// change at runtime. A reload that changes them is rejected with
// ErrRestartRequired and the running config stays, on both reload paths.
func TestMultiChainLoader_Reload_RejectsRestartOnlyChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{name: "status listen", mutate: func(y string) string { return strings.Replace(y, `listen: ":3000"`, `listen: ":3001"`, 1) }},
		{name: "auth toggled", mutate: func(y string) string { return strings.Replace(y, "auth: false", "auth: true", 1) }},
		{name: "proxy endpoint listen", mutate: func(y string) string { return strings.Replace(y, `listen: ":8081"`, `listen: ":9081"`, 1) }},
		{name: "endpoint added", mutate: func(y string) string {
			return strings.Replace(y, "      - protocol: rest\n        listen: \":8080\"", "      - protocol: rest\n        listen: \":8080\"\n      - protocol: grpc\n        listen: \":8082\"", 1)
		}},
		{name: "network renamed", mutate: func(y string) string { return strings.ReplaceAll(y, "pocket", "pocket2") }},
		{name: "rate limit enabled", mutate: func(y string) string {
			return strings.Replace(y, "auth: false\n", "auth: false\nrate_limit:\n  enabled: true\n  requests_per_second: 10\n  burst: 20\n", 1)
		}},
		{name: "redis enabled", mutate: func(y string) string {
			return strings.Replace(y, "auth: false\n", "auth: false\nredis:\n  enabled: true\n  uri: \"redis://localhost:6379\"\n", 1)
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			changed := tc.mutate(reloadedYAML)
			if changed == reloadedYAML {
				t.Fatal("mutation did not change the config; fix the test")
			}

			// SIGHUP path.
			path := writeTestConfig(t, reloadedYAML)
			l := newUnwatchedLoader(t, path)
			before := l.Get()
			rewriteConfig(t, path, changed)
			err := l.Reload()
			if !errors.Is(err, ErrRestartRequired) {
				t.Errorf("Reload err = %v, want ErrRestartRequired", err)
			}
			if l.Get() != before {
				t.Error("Reload swapped in a config that needs a restart")
			}

			// File-watcher path.
			l.onConfigChange(fsnotify.Event{Name: path, Op: fsnotify.Write})
			if l.Get() != before {
				t.Error("watcher swapped in a config that needs a restart")
			}
		})
	}
}

// TestMultiChainLoader_Reload_AllowsRuntimeChanges: nodes, timeouts, height
// check intervals and users change at runtime without a restart.
func TestMultiChainLoader_Reload_AllowsRuntimeChanges(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t, reloadedYAML)
	l := newUnwatchedLoader(t, path)

	changed := strings.Replace(withHealthCheck(reloadedYAML, "3s"), "interval: 10s", "interval: 20s", 1)
	changed = strings.Replace(changed, `      rpc: "http://seed-two:26657"`, `      rpc: "http://seed-two-new:26657"`, 1)
	rewriteConfig(t, path, changed)
	if err := l.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	cfg := l.Get()
	if cfg.Timeouts.HealthCheck != 3*time.Second {
		t.Errorf("HealthCheck = %s, want 3s", cfg.Timeouts.HealthCheck)
	}
	if got := cfg.Networks[0].HeightCheck.Interval; got != 20*time.Second {
		t.Errorf("HeightCheck.Interval = %s, want 20s", got)
	}
	if got := cfg.Internals[1].Endpoints["rpc"]; got != "http://seed-two-new:26657" {
		t.Errorf("seed-two rpc = %q, want the new URL", got)
	}
}

const twoNetworksYAML = `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: rpc
        listen: ":8081"
      - protocol: rest
        listen: ":8080"
  - name: eth
    type: evm
    endpoints:
      - protocol: jsonrpc
        listen: ":9080"
internals:
  - name: seed-one
    network: pocket
    endpoints:
      rpc: "http://seed-one:26657"
      rest: "http://seed-one:1317"
  - name: geth
    network: eth
    endpoints:
      jsonrpc: "http://geth:8545"
`

// twoNetworksReordered lists the same networks and endpoints in another order
// and moves geth to a new URL, which is a runtime change.
const twoNetworksReordered = `
listen: ":3000"
timeouts:
  health_check: 5s
  proxy: 60s
networks:
  - name: eth
    type: evm
    endpoints:
      - protocol: jsonrpc
        listen: ":9080"
  - name: pocket
    type: cosmos
    endpoints:
      - protocol: rest
        listen: ":8080"
      - protocol: rpc
        listen: ":8081"
internals:
  - name: seed-one
    network: pocket
    endpoints:
      rpc: "http://seed-one:26657"
      rest: "http://seed-one:1317"
  - name: geth
    network: eth
    endpoints:
      jsonrpc: "http://geth-new:8545"
`

// TestMultiChainLoader_Reload_ReorderIsNotARestartChange: listing the same
// networks and endpoints in another order changes no listener, so it must not
// block the runtime change that comes with it.
func TestMultiChainLoader_Reload_ReorderIsNotARestartChange(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t, twoNetworksYAML)
	l := newUnwatchedLoader(t, path)

	rewriteConfig(t, path, twoNetworksReordered)
	if err := l.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	var geth string
	for _, n := range l.Get().Internals {
		if n.Name == "geth" {
			geth = n.Endpoints["jsonrpc"]
		}
	}
	if geth != "http://geth-new:8545" {
		t.Errorf("geth jsonrpc = %q, want the new URL", geth)
	}
}

// configReloadOK reads the reload-health gauge.
func configReloadOK(t *testing.T) float64 {
	t.Helper()
	var m dto.Metric
	if err := metrics.ConfigReloadOK.Write(&m); err != nil {
		t.Fatalf("read ConfigReloadOK: %v", err)
	}
	return m.GetGauge().GetValue()
}

// TestMultiChainLoader_ReloadOKGauge: a rejected reload leaves the running
// config different from the file, which must be visible to alerting, not only
// in a log line. Not parallel: the gauge is global.
func TestMultiChainLoader_ReloadOKGauge(t *testing.T) {
	path := writeTestConfig(t, reloadedYAML)
	l := newUnwatchedLoader(t, path)

	rewriteConfig(t, path, "networks: [broken")
	if err := l.Reload(); err == nil {
		t.Fatal("Reload of a broken file: want error")
	}
	if got := configReloadOK(t); got != 0 {
		t.Errorf("after a failed reload ConfigReloadOK = %v, want 0", got)
	}

	rewriteConfig(t, path, reloadedYAML)
	if err := l.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := configReloadOK(t); got != 1 {
		t.Errorf("after a good reload ConfigReloadOK = %v, want 1", got)
	}
}
