package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfig = `
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
internals:
  - name: node-1
    network: pocket
    endpoints:
      rpc: "http://node-1:26657"
  - name: node-2
    network: pocket
    endpoints:
      rpc: "http://node-2:26657"
`

// v1Config is the pre-V2 format, which V2 rejects.
const v1Config = `
listen: ":3000"
networks:
  - name: pocket
    api_listen: ":8080"
internals:
  - name: node-1
    network: pocket
    api: "http://node-1:1317"
`

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestValidateConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "valid",
			path:       writeFile(t, validConfig),
			wantCode:   0,
			wantStdout: "valid: 1 networks, 2 internal nodes, 0 externals",
		},
		{name: "v1 format", path: writeFile(t, v1Config), wantCode: 1, wantStderr: "invalid configuration"},
		{name: "missing file", path: filepath.Join(t.TempDir(), "absent.yaml"), wantCode: 1, wantStderr: "failed to read config"},
		{name: "empty file", path: writeFile(t, ""), wantCode: 1, wantStderr: "invalid configuration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := validateConfig(tt.path, &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("exit code: got %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			if tt.wantStdout != "" {
				if !strings.Contains(stdout.String(), tt.wantStdout) {
					t.Fatalf("stdout %q does not contain %q", stdout.String(), tt.wantStdout)
				}
				if stderr.Len() != 0 {
					t.Fatalf("stderr: got %q, want empty", stderr.String())
				}
			}
			if tt.wantStderr != "" {
				if !strings.Contains(stderr.String(), tt.wantStderr) {
					t.Fatalf("stderr %q does not contain %q", stderr.String(), tt.wantStderr)
				}
				if !strings.Contains(stderr.String(), tt.path) {
					t.Fatalf("stderr %q does not name the file %q", stderr.String(), tt.path)
				}
				if stdout.Len() != 0 {
					t.Fatalf("stdout: got %q, want empty", stdout.String())
				}
			}
		})
	}
}

// TestBinaryFlags builds the binary the way the release does, with the version
// injected through -ldflags, and checks the flags an operator runs.
func TestBinaryFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	t.Parallel()

	bin := filepath.Join(t.TempDir(), "sauron")
	build := exec.Command("go", "build", "-ldflags", "-X main.version=v9.9.9-test", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	run := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		done := make(chan struct{})
		var out []byte
		var err error
		go func() { out, err = cmd.CombinedOutput(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Fatalf("%v did not exit: it must not start the server", args)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return string(out), exitErr.ExitCode()
		}
		if err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
		return string(out), 0
	}

	if out, code := run("-version"); code != 0 || strings.TrimSpace(out) != "Sauron v9.9.9-test" {
		t.Fatalf("-version: code %d, output %q", code, out)
	}
	if out, code := run("-validate", "-config", writeFile(t, validConfig)); code != 0 || !strings.Contains(out, "valid: 1 networks") {
		t.Fatalf("-validate valid: code %d, output %q", code, out)
	}
	if out, code := run("-validate", "-config", writeFile(t, v1Config)); code != 1 || !strings.Contains(out, "invalid configuration") {
		t.Fatalf("-validate v1: code %d, output %q", code, out)
	}
}

// A config that loads but leaves an endpoint without nodes is valid, with the
// warning on stderr, so an operator sees it before starting.
func TestValidateConfig_Warnings(t *testing.T) {
	t.Parallel()

	path := writeFile(t, strings.Replace(validConfig, "      - protocol: rpc\n        listen: \":8081\"\n",
		"      - protocol: rpc\n        listen: \":8081\"\n      - protocol: grpc\n        listen: \":8082\"\n", 1))
	var stdout, stderr bytes.Buffer
	if code := validateConfig(path, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code: got %d, want 0 (stderr %q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "valid: 1 networks") {
		t.Fatalf("stdout %q", stdout.String())
	}
	want := `warning: network "pocket": endpoint "grpc" has no internal node with a "grpc" URL`
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr %q does not contain %q", stderr.String(), want)
	}
}
