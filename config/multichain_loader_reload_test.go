package config

import (
	"os"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// reloadedYAML differs from validMultiChainYAML in fields that need every
// decode hook: durations, a new node and the "permissions: all" shorthand.
const reloadedYAML = `
listen: ":3000"
auth: true

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
	if !cfg.Auth || len(cfg.Users) != 1 || !cfg.Users[0].Permissions.All {
		t.Errorf("Auth/Users = %v/%+v, want auth with one user with permissions all", cfg.Auth, cfg.Users)
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
