package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestLoadMultiChainFile_Valid(t *testing.T) {
	t.Parallel()

	cfg, err := LoadMultiChainFile(writeTestConfig(t, validMultiChainYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Listen != ":3000" {
		t.Fatalf("Listen: got %q", cfg.Listen)
	}
	if len(cfg.Networks) != 1 || cfg.Networks[0].Name != "pocket" || cfg.Networks[0].Type != "cosmos" {
		t.Fatalf("Networks: got %+v", cfg.Networks)
	}
	if len(cfg.Internals) != 1 || cfg.Internals[0].Name != "seed-one" {
		t.Fatalf("Internals: got %+v", cfg.Internals)
	}
	// Defaults are applied, as for the running server.
	if cfg.ExternalFailoverThreshold != 2 {
		t.Fatalf("ExternalFailoverThreshold: got %d, want 2", cfg.ExternalFailoverThreshold)
	}
	if cfg.GRPCKeepalive.Time != 600*time.Second {
		t.Fatalf("GRPCKeepalive.Time: got %v, want 600s", cfg.GRPCKeepalive.Time)
	}
}

func TestLoadMultiChainFile_Errors(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "absent.yaml")
	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "missing file", path: missing, wantErr: "failed to read config"},
		{name: "malformed yaml", path: writeTestConfig(t, "networks: [\n"), wantErr: "failed to read config"},
		{name: "empty file", path: writeTestConfig(t, ""), wantErr: "invalid configuration"},
		{
			name:    "network without type",
			path:    writeTestConfig(t, strings.Replace(validMultiChainYAML, "    type: cosmos\n", "", 1)),
			wantErr: "invalid configuration",
		},
		{
			name:    "internal on an unknown network",
			path:    writeTestConfig(t, strings.Replace(validMultiChainYAML, "    network: pocket\n", "    network: other\n", 1)),
			wantErr: "invalid configuration",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := LoadMultiChainFile(tt.path)
			if err == nil {
				t.Fatalf("expected error, got config %+v", cfg)
			}
			if cfg != nil {
				t.Fatalf("expected nil config with error, got %+v", cfg)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadMultiChainFile_RejectsUnknownKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		wantKey string
	}{
		{
			name:    "misspelled top-level key",
			yaml:    strings.Replace(validMultiChainYAML, "auth: false\n", "auth: false\nexternal_failover_treshold: 3\n", 1),
			wantKey: "external_failover_treshold",
		},
		{
			name:    "unknown key on an internal node",
			yaml:    strings.Replace(validMultiChainYAML, "    network: pocket\n", "    network: pocket\n    grpc_insecur: true\n", 1),
			wantKey: "grpc_insecur",
		},
		{
			name:    "V1 key on a network",
			yaml:    strings.Replace(validMultiChainYAML, "    type: cosmos\n", "    type: cosmos\n    api_listen: \":8080\"\n", 1),
			wantKey: "api_listen",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := LoadMultiChainFile(writeTestConfig(t, tt.yaml))
			if err == nil {
				t.Fatalf("expected error, got config %+v", cfg)
			}
			if !strings.Contains(err.Error(), "unknown keys") || !strings.Contains(err.Error(), tt.wantKey) {
				t.Fatalf("error %q does not name unknown key %q", err, tt.wantKey)
			}
		})
	}
}

// The running server must keep starting with keys it does not know, as it
// did before: only the check is strict.
func TestMultiChainLoader_UnknownKeysStillLoad(t *testing.T) {
	t.Parallel()

	yaml := strings.Replace(validMultiChainYAML, "auth: false\n", "auth: false\nexternal_failover_treshold: 3\n", 1)
	core, logs := observer.New(zap.WarnLevel)
	if _, err := NewMultiChainLoader(writeTestConfig(t, yaml), zap.New(core)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entries := logs.FilterMessage("Unknown configuration keys ignored").All()
	if len(entries) != 1 {
		t.Fatalf("want 1 warning about unknown keys, got %d: %+v", len(entries), logs.All())
	}
	keys, ok := entries[0].ContextMap()["keys"].([]interface{})
	if !ok || len(keys) != 1 || keys[0] != "external_failover_treshold" {
		t.Fatalf("keys field: got %#v", entries[0].ContextMap()["keys"])
	}
}

func TestLoadMultiChainFile_GRPCAddressScheme(t *testing.T) {
	t.Parallel()

	withGRPC := func(nodeAddr, advertise string) string {
		y := strings.Replace(validMultiChainYAML, "      - protocol: rest\n        listen: \":8080\"\n",
			"      - protocol: rest\n        listen: \":8080\"\n      - protocol: grpc\n        listen: \":8082\"\n"+advertise, 1)
		return strings.Replace(y, `      rest: "http://seed-one:1317"`+"\n", `      rest: "http://seed-one:1317"`+"\n"+`      grpc: "`+nodeAddr+`"`+"\n", 1)
	}
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{name: "host and port", yaml: withGRPC("seed-one:9090", "")},
		{name: "dns resolver target", yaml: withGRPC("dns:///seed-one:9090", "")},
		{name: "https scheme on node", yaml: withGRPC("https://seed-one:443", ""), wantErr: `internal node "seed-one": grpc "https://seed-one:443" must be host:port without a scheme`},
		{name: "http scheme on node", yaml: withGRPC("http://seed-one:9090", ""), wantErr: "without a scheme"},
		{name: "https scheme on advertise", yaml: withGRPC("seed-one:9090", "        advertise: \"https://grpc.example.com:443\"\n"), wantErr: "without a scheme"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadMultiChainFile(writeTestConfig(t, tt.yaml))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %v does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigWarnings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{
			name: "one internal node: no failover",
			yaml: validMultiChainYAML,
			want: []string{`network "pocket": 1 internal node; if it fails`},
		},
		{
			name: "two internal nodes, every endpoint served",
			yaml: validMultiChainYAML + `  - name: seed-two
    network: pocket
    endpoints:
      rpc: "http://seed-two:26657"
      rest: "http://seed-two:1317"
`,
		},
		{
			name: "misspelled node URL key leaves an endpoint without nodes",
			yaml: strings.Replace(validMultiChainYAML, `      rest: "http://seed-one:1317"`, `      api: "http://seed-one:1317"`, 1),
			want: []string{`network "pocket": endpoint "rest" has no internal node with a "rest" URL`, `network "pocket": 1 internal node`},
		},
		{
			name: "misspelled endpoint protocol",
			yaml: strings.Replace(validMultiChainYAML, "      - protocol: rest\n", "      - protocol: rests\n", 1),
			want: []string{`network "pocket": endpoint "rests" has no internal node with a "rests" URL`, `network "pocket": 1 internal node`},
		},
		{
			// Network "other" serves grpc; the only grpc URL is on a node of
			// network "pocket".
			name: "a node of another network does not count",
			yaml: strings.Replace(validMultiChainYAML, "internals:\n", `  - name: other
    type: cosmos
    endpoints:
      - protocol: grpc
        listen: ":9092"

internals:
  - name: other-node
    network: pocket
    endpoints:
      grpc: "other-node:9090"
`, 1),
			want: []string{`network "other": no internal node`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := LoadMultiChainFile(writeTestConfig(t, tt.yaml))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := ConfigWarnings(cfg)
			if len(got) != len(tt.want) {
				t.Fatalf("warnings: got %q, want %q", got, tt.want)
			}
			for i := range got {
				if !strings.HasPrefix(got[i], tt.want[i]) {
					t.Fatalf("warning %d: got %q, want prefix %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// A gRPC address with a scheme loaded before this check existed; the server
// must keep loading it, with a warning.
func TestMultiChainLoader_GRPCSchemeStillLoads(t *testing.T) {
	t.Parallel()

	yaml := strings.Replace(validMultiChainYAML, `      rest: "http://seed-one:1317"`+"\n",
		`      rest: "http://seed-one:1317"`+"\n"+`      grpc: "https://seed-one:443"`+"\n", 1)
	core, logs := observer.New(zap.WarnLevel)
	if _, err := NewMultiChainLoader(writeTestConfig(t, yaml), zap.New(core)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entries := logs.FilterMessage("Configuration problem").All()
	if len(entries) != 1 {
		t.Fatalf("want 1 configuration problem, got %d: %+v", len(entries), logs.All())
	}
	if p, _ := entries[0].ContextMap()["problem"].(string); !strings.Contains(p, `"https://seed-one:443"`) {
		t.Fatalf("problem field: got %q", p)
	}
}

// A V1 file fails validation on whichever V2 field it lacks first; the error
// also names its V1 keys, which is what tells the reader the format is wrong.
func TestLoadMultiChainFile_InvalidFileAlsoNamesUnknownKeys(t *testing.T) {
	t.Parallel()

	v1 := "listen: \":3000\"\nnetworks:\n  - name: pocket\n    api_listen: \":8080\"\ninternals:\n  - name: n\n    network: pocket\n    api: \"http://n:1317\"\n"
	_, err := LoadMultiChainFile(writeTestConfig(t, v1))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"invalid configuration", "unknown keys", "networks[0].api_listen", "internals[0].api"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

// Reloading an unchanged file must not repeat its warnings; a reload that
// changes them logs the new set.
func TestMultiChainLoader_ReloadLogsWarningsOnlyWhenTheyChange(t *testing.T) {
	t.Parallel()

	path := writeTestConfig(t, validMultiChainYAML)
	core, logs := observer.New(zap.WarnLevel)
	l := &MultiChainLoader{logger: zap.New(core), v: newConfigViper(path), path: path}
	if err := l.v.ReadInConfig(); err != nil {
		t.Fatalf("ReadInConfig: %v", err)
	}
	cfg, err := parseMultiChain(l.v)
	if err != nil {
		t.Fatalf("parseMultiChain: %v", err)
	}
	l.logWarnings(l.v, cfg)
	l.config.Store(cfg)

	count := func() int { return len(logs.FilterMessage("Configuration warning").All()) }
	if got := count(); got != 1 {
		t.Fatalf("after load: %d warnings, want 1 (1 internal node)", got)
	}
	for i := 0; i < 2; i++ {
		if err := l.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}
	}
	if got := count(); got != 1 {
		t.Fatalf("after 2 unchanged reloads: %d warnings, want still 1", got)
	}

	rewriteConfig(t, path, strings.Replace(validMultiChainYAML, `      rest: "http://seed-one:1317"`, `      api: "http://seed-one:1317"`, 1))
	if err := l.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := count(); got != 3 {
		t.Fatalf("after a reload that changes them: %d warnings, want 3 (1 + the new set of 2)", got)
	}
}
