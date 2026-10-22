package config

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// OnReload subscribers run after every reload that changes the running
// config, with that config, in registration order; never after a rejected file.
func TestMultiChainLoader_OnReload(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t, validMultiChainYAML)
	loader, err := NewMultiChainLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("NewMultiChainLoader: %v", err)
	}
	var mu sync.Mutex
	var calls []string
	var got []*MultiChainConfig
	loader.OnReload(func(cfg *MultiChainConfig) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, "first")
		got = append(got, cfg)
	})
	loader.OnReload(func(*MultiChainConfig) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, "second")
	})

	// pairsSince checks that calls after index from are whole first,second
	// pairs (the file watcher may add reloads of its own) and returns their count.
	pairsSince := func(from int) int {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		tail := calls[from:]
		if len(tail)%2 != 0 {
			t.Fatalf("calls %v: not whole pairs", calls)
		}
		for i := 0; i < len(tail); i += 2 {
			if tail[i] != "first" || tail[i+1] != "second" {
				t.Fatalf("calls %v: subscribers out of registration order", calls)
			}
		}
		return len(tail) / 2
	}
	lastGot := func() *MultiChainConfig {
		mu.Lock()
		defer mu.Unlock()
		return got[len(got)-1]
	}
	callCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(calls)
	}

	// Applied reload.
	rewriteConfig(t, path, reloadedYAML)
	if err := loader.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if n := pairsSince(0); n < 1 {
		t.Fatal("subscribers not called after an applied reload")
	}
	if lastGot() != loader.Get() {
		t.Error("subscriber did not get the stored config")
	}

	// The file watcher reloads on its own after each write; wait until it is
	// quiet, so a late reload of the good file is not counted below.
	waitQuiet := func() {
		t.Helper()
		last, quietSince := callCount(), time.Now()
		for time.Since(quietSince) < 300*time.Millisecond {
			time.Sleep(20 * time.Millisecond)
			if n := callCount(); n != last {
				last, quietSince = n, time.Now()
			}
		}
	}

	// Rejected file: no call.
	waitQuiet()
	before := callCount()
	rewriteConfig(t, path, "networks: [\n  bad")
	if err := loader.Reload(); err == nil {
		t.Fatal("Reload accepted a malformed file")
	}
	if n := callCount(); n != before {
		t.Errorf("calls after a rejected file = %d, want still %d", n, before)
	}

	// Startup-only change: the rest is applied, so subscribers run with it.
	before = callCount()
	rewriteConfig(t, path, strings.Replace(reloadedYAML, `listen: ":3000"`, `listen: ":3999"`, 1))
	if err := loader.Reload(); !errors.Is(err, ErrRestartRequired) {
		t.Fatalf("Reload err = %v, want ErrRestartRequired", err)
	}
	if n := pairsSince(before); n < 1 {
		t.Fatal("subscribers not called after a restart-required reload")
	}
	if cfg := lastGot(); cfg != loader.Get() || cfg.Listen != ":3000" {
		t.Errorf("last config listen %q, stored %v; want the stored config with listen kept at :3000", cfg.Listen, cfg == loader.Get())
	}
}

// Registering while reloads run is safe.
func TestMultiChainLoader_OnReload_Concurrent(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t, validMultiChainYAML)
	loader, err := NewMultiChainLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("NewMultiChainLoader: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			loader.OnReload(func(*MultiChainConfig) {})
		}()
		go func() {
			defer wg.Done()
			_ = loader.Reload()
		}()
	}
	wg.Wait()
}
