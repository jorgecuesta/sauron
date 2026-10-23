package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A node whose REST is down while its RPC and gRPC answer shows up in
// sauron_node_available per protocol (rest 0, rpc 1, grpc 1), so an alert on
// it fires. Before, only the height-check protocol (rpc) was exported.
func TestPipeline_AvailabilityPerProtocol(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real servers")
	}
	t.Parallel()

	name := fmt.Sprintf("avail-%d", pipelineSeq.Add(1))
	pn := startFakeNode(t, name)
	status, rest, rpc, grpcAddr := freeAddr(t), freeAddr(t), freeAddr(t), freeAddr(t)
	startConfig(t, fmt.Sprintf(`
listen: "%s"
timeouts:
  health_check: 1s
  proxy: 5s
networks:
  - name: pocket
    type: cosmos
    height_check:
      interval: 1s
    endpoints:
      - protocol: rest
        listen: "%s"
      - protocol: rpc
        listen: "%s"
      - protocol: grpc
        listen: "%s"
internals:
  - name: %s
    network: pocket
    grpc_insecure: true
    endpoints:
      rest: "http://127.0.0.1:1"
      rpc: "http://%s"
      grpc: "%s"
`, status, rest, rpc, grpcAddr, name, pn.rpc, pn.grpc))

	want := map[string]string{"rest": "0", "rpc": "1", "grpc": "1"}
	deadline := time.Now().Add(15 * time.Second)
	var got map[string]string
	for time.Now().Before(deadline) {
		got = availability(t, status, name)
		if got["rest"] == want["rest"] && got["rpc"] == want["rpc"] && got["grpc"] == want["grpc"] {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("sauron_node_available for %s: got %v, want %v", name, got, want)
}

// availability returns sauron_node_available{node=name} by type, as exported.
func availability(t *testing.T, statusAddr, name string) map[string]string {
	t.Helper()
	resp, err := http.Get("http://" + statusAddr + "/metrics")
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "sauron_node_available{") || !strings.Contains(line, `node="`+name+`"`) {
			continue
		}
		for _, typ := range []string{"rest", "rpc", "grpc"} {
			if strings.Contains(line, `type="`+typ+`"`) {
				f := strings.Fields(line)
				out[typ] = f[len(f)-1]
			}
		}
	}
	return out
}
