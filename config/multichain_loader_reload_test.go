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

// reloadedYAML differs from validMultiChainYAML only in fields that reload
// live, and needs every decode hook: durations, a new node and the
// "permissions: all" shorthand.
const reloadedYAML = `
listen: ":3000"
auth: false

timeouts:
  health_check: 7s
  proxy: 60s

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

// startupBaseYAML exercises every startup-only field: listeners, auth, rate
// limit, redis, proxy timeout, keepalive, archival, sync/oracles, mode,
// height_check and grpc_insecure.
const startupBaseYAML = `
listen: ":3000"
auth: false
timeouts:
  health_check: 5s
  proxy: 60s
grpc_keepalive:
  time: 600s
  timeout: 20s
networks:
  - name: pocket
    type: cosmos
    height_check:
      protocol: rpc
      interval: 30s
    endpoints:
      - protocol: rpc
        listen: ":8081"
      - protocol: grpc
        listen: ":8082"
        advertise: "sauron-grpc:443"
    archival:
      min_height: 100
    sync:
      max_drift: 5
      oracles:
        - url: "https://oracle-a.example"
  - name: eth
    type: evm
    mode: latest
    endpoints:
      - protocol: jsonrpc
        listen: ":9080"
internals:
  - name: seed-one
    network: pocket
    grpc_insecure: true
    endpoints:
      rpc: "http://seed-one:26657"
      grpc: "seed-one:9090"
  - name: geth
    network: eth
    endpoints:
      jsonrpc: "http://geth:8545"
`

// withRuntimeChange adds a change that is always live (a node URL), so each
// case also proves the runtime part of the same file is applied.
func withRuntimeChange(y string) string {
	return strings.Replace(y, `rpc: "http://seed-one:26657"`, `rpc: "http://seed-one-new:26657"`, 1)
}

func findNetwork(cfg *MultiChainConfig, name string) *MultiChainNetwork {
	for i := range cfg.Networks {
		if cfg.Networks[i].Name == name {
			return &cfg.Networks[i]
		}
	}
	return nil
}

// TestMultiChainLoader_Reload_KeepsStartupOnlyFieldsAppliesTheRest: a reload
// that changes a field the running server only reads at startup keeps the
// running value of that field, applies everything else in the file, reports
// ErrRestartRequired and sets sauron_config_reload_ok to 0. Not parallel: the
// gauge is global.
func TestMultiChainLoader_Reload_KeepsStartupOnlyFieldsAppliesTheRest(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(string) string
		wantField string                                    // named in the error
		kept      func(t *testing.T, cfg *MultiChainConfig) // running value still in place
	}{
		{"status listen", func(y string) string { return strings.Replace(y, `listen: ":3000"`, `listen: ":3001"`, 1) }, "listen",
			func(t *testing.T, c *MultiChainConfig) { eq(t, c.Listen, ":3000") }},
		{"auth", func(y string) string {
			return strings.Replace(y, "auth: false", "auth: true\nusers:\n  - name: u\n    token: t\n    permissions: all", 1)
		}, "auth", func(t *testing.T, c *MultiChainConfig) { eq(t, c.Auth, false) }},
		{"rate limit", func(y string) string {
			return strings.Replace(y, "auth: false\n", "auth: false\nrate_limit:\n  enabled: true\n  requests_per_second: 10\n  burst: 20\n", 1)
		}, "rate_limit", func(t *testing.T, c *MultiChainConfig) { eq(t, c.RateLimit.Enabled, false) }},
		{"redis", func(y string) string {
			return strings.Replace(y, "auth: false\n", "auth: false\nredis:\n  enabled: true\n  uri: \"redis://r:6379\"\n", 1)
		}, "redis", func(t *testing.T, c *MultiChainConfig) { eq(t, c.Redis.Enabled, false) }},
		{"proxy timeout", func(y string) string { return strings.Replace(y, "proxy: 60s", "proxy: 5s", 1) }, "timeouts.proxy",
			func(t *testing.T, c *MultiChainConfig) { eq(t, c.Timeouts.Proxy, 60*time.Second) }},
		{"grpc keepalive", func(y string) string { return strings.Replace(y, "time: 600s", "time: 30s", 1) }, "grpc_keepalive",
			func(t *testing.T, c *MultiChainConfig) { eq(t, c.GRPCKeepalive.Time, 600*time.Second) }},
		{"endpoint listener", func(y string) string { return strings.Replace(y, `listen: ":8081"`, `listen: ":9081"`, 1) }, "networks[pocket].endpoints",
			func(t *testing.T, c *MultiChainConfig) { eq(t, findNetwork(c, "pocket").Endpoints[0].Listen, ":8081") }},
		{"archival removed", func(y string) string { return strings.Replace(y, "    archival:\n      min_height: 100\n", "", 1) }, "networks[pocket].archival",
			func(t *testing.T, c *MultiChainConfig) {
				if findNetwork(c, "pocket").Archival == nil {
					t.Error("archival block dropped although the filter is still wired")
				}
			}},
		{"archival added", func(y string) string {
			return strings.Replace(y, "    mode: latest\n", "    mode: latest\n    archival:\n      min_height: 1\n", 1)
		}, "networks[eth].archival", func(t *testing.T, c *MultiChainConfig) {
			if findNetwork(c, "eth").Archival != nil {
				t.Error("archival added although no filter or cron would serve it")
			}
		}},
		{"sync max drift", func(y string) string { return strings.Replace(y, "max_drift: 5", "max_drift: 50", 1) }, "networks[pocket].sync",
			func(t *testing.T, c *MultiChainConfig) { eq(t, findNetwork(c, "pocket").Sync.MaxDrift, int64(5)) }},
		{"oracle url", func(y string) string { return strings.Replace(y, "oracle-a.example", "oracle-b.example", 1) }, "networks[pocket].sync",
			func(t *testing.T, c *MultiChainConfig) {
				eq(t, findNetwork(c, "pocket").Sync.Oracles[0].URL, "https://oracle-a.example")
			}},
		{"mode", func(y string) string { return strings.Replace(y, "mode: latest", "mode: finalized", 1) }, "networks[eth].mode",
			func(t *testing.T, c *MultiChainConfig) { eq(t, findNetwork(c, "eth").Mode, "latest") }},
		{"height check protocol", func(y string) string {
			return strings.Replace(y, "protocol: rpc\n      interval", "protocol: grpc\n      interval", 1)
		}, "networks[pocket].height_check",
			func(t *testing.T, c *MultiChainConfig) { eq(t, findNetwork(c, "pocket").HeightCheck.Protocol, "rpc") }},
		{"network removed", func(y string) string {
			y = strings.Replace(y, "  - name: eth\n    type: evm\n    mode: latest\n    endpoints:\n      - protocol: jsonrpc\n        listen: \":9080\"\n", "", 1)
			return strings.Replace(y, "  - name: geth\n    network: eth\n    endpoints:\n      jsonrpc: \"http://geth:8545\"\n", "", 1)
		}, "networks", func(t *testing.T, c *MultiChainConfig) {
			if findNetwork(c, "eth") == nil {
				t.Error("removed network dropped although its listener is still serving")
			}
		}},
		{"network added", func(y string) string {
			return strings.Replace(y, "internals:\n", "  - name: sol\n    type: solana\n    endpoints:\n      - protocol: jsonrpc\n        listen: \":9180\"\ninternals:\n", 1)
		}, "networks", func(t *testing.T, c *MultiChainConfig) {
			if findNetwork(c, "sol") != nil {
				t.Error("added network applied although it has no listener")
			}
		}},
		{"network type", func(y string) string { return strings.Replace(y, "type: evm", "type: solana", 1) }, "networks[eth].type",
			func(t *testing.T, c *MultiChainConfig) { eq(t, findNetwork(c, "eth").Type, "evm") }},
		{"grpc message size", func(y string) string {
			return strings.Replace(y, "        advertise: \"sauron-grpc:443\"\n", "        advertise: \"sauron-grpc:443\"\n        grpc_max_recv_msg_size: 1048576\n", 1)
		}, "networks[pocket].endpoints", func(t *testing.T, c *MultiChainConfig) {
			eq(t, findNetwork(c, "pocket").Endpoints[1].GRPCMaxRecvMsgSize, 0)
		}},
		{"endpoint removed", func(y string) string {
			return strings.Replace(y, "      - protocol: grpc\n        listen: \":8082\"\n        advertise: \"sauron-grpc:443\"\n", "", 1)
		}, "networks[pocket].endpoints", func(t *testing.T, c *MultiChainConfig) {
			eq(t, len(findNetwork(c, "pocket").Endpoints), 2)
		}},
		{"grpc_insecure with same url", func(y string) string { return strings.Replace(y, "grpc_insecure: true", "grpc_insecure: false", 1) }, "internals[seed-one].grpc_insecure",
			func(t *testing.T, c *MultiChainConfig) { eq(t, c.Internals[0].GRPCInsecure, true) }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changed := withRuntimeChange(tc.mutate(startupBaseYAML))
			if tc.mutate(startupBaseYAML) == startupBaseYAML {
				t.Fatal("mutation did not change the config; fix the test")
			}

			path := writeTestConfig(t, startupBaseYAML)
			l := newUnwatchedLoader(t, path)
			rewriteConfig(t, path, changed)

			err := l.Reload()
			if !errors.Is(err, ErrRestartRequired) {
				t.Fatalf("Reload err = %v, want ErrRestartRequired", err)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error %q does not name %q", err, tc.wantField)
			}
			cfg := l.Get()
			tc.kept(t, cfg)
			var seedOne string
			for _, n := range cfg.Internals {
				if n.Name == "seed-one" {
					seedOne = n.Endpoints["rpc"]
				}
			}
			eq(t, seedOne, "http://seed-one-new:26657")
			if got := configReloadOK(t); got != 0 {
				t.Errorf("ConfigReloadOK = %v, want 0 while a restart is pending", got)
			}
		})
	}
}

// TestMultiChainLoader_Reload_AdvertiseIsLive: an endpoint's advertise URL is
// read per request, so changing it is a plain runtime change.
func TestMultiChainLoader_Reload_AdvertiseIsLive(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t, startupBaseYAML)
	l := newUnwatchedLoader(t, path)
	rewriteConfig(t, path, strings.Replace(startupBaseYAML, `advertise: "sauron-grpc:443"`, `advertise: "sauron-grpc-new:443"`, 1))
	if err := l.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	eq(t, findNetwork(l.Get(), "pocket").Endpoints[1].Advertise, "sauron-grpc-new:443")
}

func eq[T comparable](t *testing.T, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("got %v, want %v", got, want)
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

// TestMultiChainLoader_Reload_RejectsWhenKeptConfigIsInvalid: keeping the
// running network set can leave the rest of the new file inconsistent (here a
// node on a network that cannot be added without a restart). Then nothing is
// applied and the error is not ErrRestartRequired.
func TestMultiChainLoader_Reload_RejectsWhenKeptConfigIsInvalid(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t, startupBaseYAML)
	l := newUnwatchedLoader(t, path)
	before := l.Get()

	changed := strings.Replace(startupBaseYAML, "internals:\n", "  - name: sol\n    type: solana\n    endpoints:\n      - protocol: jsonrpc\n        listen: \":9180\"\ninternals:\n  - name: sol-node\n    network: sol\n    endpoints:\n      jsonrpc: \"http://sol:8899\"\n", 1)
	rewriteConfig(t, path, withRuntimeChange(changed))

	err := l.Reload()
	if err == nil || errors.Is(err, ErrRestartRequired) {
		t.Fatalf("Reload err = %v, want a validation error that is not ErrRestartRequired", err)
	}
	if l.Get() != before {
		t.Error("config replaced although the kept config was invalid")
	}
}

// TestMultiChainLoader_Reload_CircuitBreakerAndArchivalMinHeightAreLive:
// circuit breakers are read per request and archival.min_height per archival
// tick, so both apply without a restart.
func TestMultiChainLoader_Reload_CircuitBreakerAndArchivalMinHeightAreLive(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t, startupBaseYAML)
	l := newUnwatchedLoader(t, path)

	changed := strings.Replace(startupBaseYAML, "min_height: 100", "min_height: 200", 1)
	changed = strings.Replace(changed, "    mode: latest\n", "    mode: latest\n    circuit_breaker:\n      http_codes: [502, 503]\n      threshold: 7\n", 1)
	rewriteConfig(t, path, changed)
	if err := l.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	eq(t, findNetwork(l.Get(), "pocket").Archival.MinHeight, int64(200))
	cb := findNetwork(l.Get(), "eth").CircuitBreaker
	if cb == nil || cb.Threshold != 7 {
		t.Errorf("eth circuit_breaker = %+v, want threshold 7", cb)
	}
}
