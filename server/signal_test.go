package server

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"sauron/config"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const signalTestYAML = `
listen: ":3000"
timeouts:
  health_check: %s
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
`

func writeSignalTestConfig(t *testing.T, healthCheck string) string {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte(sprintfYAML(healthCheck)), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func sprintfYAML(healthCheck string) string {
	return fmt.Sprintf(signalTestYAML, healthCheck)
}

// startSignalLoop runs waitForSignals on a minimal Server and returns the
// signal channel, the error channel and a channel closed when the loop returns.
func startSignalLoop(t *testing.T, loader *config.MultiChainLoader, logger *zap.Logger) (chan os.Signal, chan error, chan struct{}) {
	t.Helper()
	s := &Server{configLoader: loader, logger: logger, errCh: make(chan error, 1)}
	sigCh := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() {
		s.waitForSignals(sigCh)
		close(done)
	}()
	return sigCh, s.errCh, done
}

func TestServer_SIGHUPReloadsConfigAndKeepsRunning(t *testing.T) {
	t.Parallel()

	path := writeSignalTestConfig(t, "5s")
	loader, err := config.NewMultiChainLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("NewMultiChainLoader: %v", err)
	}
	sigCh, _, done := startSignalLoop(t, loader, zap.NewNop())

	if err := os.WriteFile(path, []byte(sprintfYAML("9s")), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	sigCh <- syscall.SIGHUP

	// The file watcher may also pick the change up; either way the new value
	// must be live while the loop keeps running.
	deadline := time.Now().Add(3 * time.Second)
	for loader.Get().Timeouts.HealthCheck != 9*time.Second {
		if time.Now().After(deadline) {
			t.Fatalf("HealthCheck after SIGHUP = %s, want 9s", loader.Get().Timeouts.HealthCheck)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("signal loop returned on SIGHUP; it must keep running")
	case <-time.After(100 * time.Millisecond):
	}

	sigCh <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("signal loop did not return on SIGTERM")
	}
}

func TestServer_SIGHUPWithBadConfigKeepsRunningAndPrevious(t *testing.T) {
	t.Parallel()

	path := writeSignalTestConfig(t, "5s")
	loader, err := config.NewMultiChainLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("NewMultiChainLoader: %v", err)
	}
	core, logs := observer.New(zapcore.ErrorLevel)
	sigCh, _, done := startSignalLoop(t, loader, zap.New(core))

	if err := os.WriteFile(path, []byte("networks: [broken"), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	sigCh <- syscall.SIGHUP

	deadline := time.Now().Add(3 * time.Second)
	for logs.Len() == 0 { // any Error-level entry: the failed reload was reported
		if time.Now().After(deadline) {
			t.Fatal("failed SIGHUP reload was not reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := loader.Get().Timeouts.HealthCheck; got != 5*time.Second {
		t.Errorf("HealthCheck = %s, want previous 5s", got)
	}
	select {
	case <-done:
		t.Fatal("signal loop returned after a failed reload")
	case <-time.After(100 * time.Millisecond):
	}
	sigCh <- syscall.SIGINT
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("signal loop did not return on SIGINT")
	}
}

func TestServer_SignalLoopReturnsOnFatalError(t *testing.T) {
	t.Parallel()

	path := writeSignalTestConfig(t, "5s")
	loader, err := config.NewMultiChainLoader(path, zap.NewNop())
	if err != nil {
		t.Fatalf("NewMultiChainLoader: %v", err)
	}
	_, errCh, done := startSignalLoop(t, loader, zap.NewNop())

	errCh <- os.ErrClosed
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("signal loop did not return on a fatal background error")
	}
}

// TestServer_SIGHUPDuringStartupDoesNotKill: between New and WaitForShutdown
// (config load, Redis ping, listener start) a SIGHUP must not take Go's
// default action and kill the process. It runs New in a child process that
// sends itself SIGHUP; the child must exit normally.
func TestServer_SIGHUPDuringStartupDoesNotKill(t *testing.T) {
	if os.Getenv("SAURON_SIGHUP_CHILD") == "1" {
		path := writeSignalTestConfig(t, "5s")
		if _, err := New(path); err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatalf("kill: %v", err)
		}
		time.Sleep(300 * time.Millisecond) // give a default-action SIGHUP time to kill us
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestServer_SIGHUPDuringStartupDoesNotKill$", "-test.count=1")
	cmd.Env = append(os.Environ(), "SAURON_SIGHUP_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process did not survive a SIGHUP after New: %v\n%s", err, out)
	}
}
