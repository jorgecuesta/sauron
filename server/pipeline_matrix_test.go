package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// rawBytesCodec passes gRPC frames through as []byte, like a client of a
// service it has no proto types for.
type rawBytesCodec struct{}

func (rawBytesCodec) Marshal(v any) ([]byte, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, errors.New("rawBytesCodec: want *[]byte")
	}
	return *b, nil
}

func (rawBytesCodec) Unmarshal(data []byte, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return errors.New("rawBytesCodec: want *[]byte")
	}
	*b = append((*b)[:0], data...)
	return nil
}

func (rawBytesCodec) Name() string { return "proto" }

func (p *pipeline) grpcConn(t *testing.T) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient("passthrough:///"+p.grpcAd, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawBytesCodec{})))
	if err != nil {
		t.Fatalf("grpc client: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

// protoRow drives one client protocol through the pipeline.
type protoRow struct {
	name string
	// typ is the node_requests "type" label the proxy records.
	typ string
	// call makes one ordinary request and returns the node that served it.
	call func(t *testing.T, p *pipeline) (node string, err error)
	// backendError makes a request the node answers with an error status and
	// checks that the client gets that status from that node.
	backendError func(t *testing.T, p *pipeline)
	// clientGone starts a request the node holds open, and gives up on it.
	clientGone func(t *testing.T, p *pipeline)
	// breakerProto is the protocol the circuit breaker counts this row under.
	breakerProto string
}

func httpRow(name, typ string, base func(*pipeline) string, path, method string) protoRow {
	return protoRow{
		name: name, typ: typ, breakerProto: typ,
		call: func(t *testing.T, p *pipeline) (string, error) {
			var code int
			var node, body string
			if method == http.MethodPost {
				code, node, body = httpPost(t, base(p)+"/", `{"jsonrpc":"2.0","id":1,"method":"status"}`)
			} else {
				code, node, body = httpGet(t, base(p)+path)
			}
			if code != http.StatusOK {
				return "", errors.New("HTTP " + http.StatusText(code) + ": " + body)
			}
			return node, nil
		},
		backendError: func(t *testing.T, p *pipeline) {
			if method == http.MethodPost {
				code, node, body := httpPost(t, base(p)+"/", `{"jsonrpc":"2.0","id":9,"method":"no_such_method"}`)
				if code != http.StatusOK || node == "" || !strings.Contains(body, "-32601") {
					t.Errorf("unknown JSON-RPC method: HTTP %d, node %q, body %s; want the node's -32601 error", code, node, body)
				}
				return
			}
			code, node, _ := httpGet(t, base(p)+"/no/such/path")
			if code != http.StatusNotFound || node == "" {
				t.Errorf("unknown path: HTTP %d from node %q, want the node's 404", code, node)
			}
		},
		clientGone: func(t *testing.T, p *pipeline) {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base(p)+"/slow", nil)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_ = resp.Body.Close()
				t.Fatalf("/slow answered %d before the client gave up", resp.StatusCode)
			}
		},
	}
}

var wsDialer = websocket.Dialer{HandshakeTimeout: 3 * time.Second}

var wsRow = protoRow{
	name: "websocket", typ: "rpc", breakerProto: "rpc",
	call: func(t *testing.T, p *pipeline) (string, error) {
		c, resp, err := wsDialer.Dial("ws://"+p.rpc+"/websocket", nil)
		if err != nil {
			return "", err
		}
		defer func() { _ = c.Close() }()
		if err := c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "subscribe", "params": map[string]string{"query": "tm.event='NewBlock'"}}); err != nil {
			return "", err
		}
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		var ack map[string]any
		if err := c.ReadJSON(&ack); err != nil {
			return "", err
		}
		return resp.Header.Get("X-Fake-Node"), nil
	},
	backendError: func(t *testing.T, p *pipeline) {
		_, resp, err := wsDialer.Dial("ws://"+p.rpc+"/no-such-websocket", nil)
		if err == nil {
			t.Fatal("upgrade on an unknown path succeeded")
		}
		if resp == nil || resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Fake-Node") == "" {
			t.Errorf("upgrade on an unknown path: %v, want the node's 404", resp)
		}
	},
	clientGone: func(t *testing.T, p *pipeline) {
		c, _, err := wsDialer.Dial("ws://"+p.rpc+"/websocket", nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		_ = c.UnderlyingConn().Close() // drop without a close frame
	},
}

func grpcUnaryCall(t *testing.T, p *pipeline, method string, timeout time.Duration) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	in, out := []byte("q"), []byte{}
	var hdr metadata.MD
	err := p.grpcConn(t).Invoke(ctx, method, &in, &out, grpc.Header(&hdr))
	if got := hdr.Get("x-cosmos-block-height"); err == nil && (len(got) != 1 || got[0] != "1000") {
		t.Errorf("x-cosmos-block-height = %v, want [1000] passed through", got)
	}
	node := ""
	if v := hdr.Get("x-fake-node"); len(v) == 1 {
		node = v[0]
	}
	return node, err
}

var grpcUnaryRow = protoRow{
	name: "grpc unary", typ: "grpc", breakerProto: "grpc",
	call: func(t *testing.T, p *pipeline) (string, error) {
		return grpcUnaryCall(t, p, "/pocket.session.Query/GetSession", 3*time.Second)
	},
	backendError: func(t *testing.T, p *pipeline) {
		node, err := grpcUnaryCall(t, p, "/fake.Service/NotFound", 3*time.Second)
		if status.Code(err) != codes.NotFound || node == "" {
			t.Errorf("NotFound method: %v from node %q, want the node's NotFound with its metadata", err, node)
		}
	},
	clientGone: func(t *testing.T, p *pipeline) {
		if _, err := grpcUnaryCall(t, p, "/fake.Service/Slow", 200*time.Millisecond); status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("Slow method: %v, want the client's DeadlineExceeded", err)
		}
	},
}

var grpcStreamRow = protoRow{
	name: "grpc server streaming", typ: "grpc", breakerProto: "grpc",
	call: func(t *testing.T, p *pipeline) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		stream, err := p.grpcConn(t).NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/fake.Service/Stream")
		if err != nil {
			return "", err
		}
		in := []byte("q")
		if err := stream.SendMsg(&in); err != nil {
			return "", err
		}
		_ = stream.CloseSend()
		msgs := 0
		for {
			var out []byte
			if err := stream.RecvMsg(&out); err != nil {
				if err != io.EOF {
					return "", err
				}
				break
			}
			msgs++
		}
		if msgs != 3 {
			return "", errors.New("stream did not deliver its 3 messages")
		}
		hdr, _ := stream.Header()
		if v := hdr.Get("x-fake-node"); len(v) == 1 {
			return v[0], nil
		}
		return "", errors.New("no x-fake-node metadata")
	},
	backendError: grpcUnaryRow.backendError,
	clientGone:   grpcUnaryRow.clientGone,
}

func matrixRows() []protoRow {
	return []protoRow{
		httpRow("rest", "rest", func(p *pipeline) string { return "http://" + p.rest }, "/cosmos/base/tendermint/v1beta1/blocks/latest", http.MethodGet),
		httpRow("rpc get", "rpc", func(p *pipeline) string { return "http://" + p.rpc }, "/status", http.MethodGet),
		httpRow("json-rpc post", "rpc", func(p *pipeline) string { return "http://" + p.rpc }, "/", http.MethodPost),
		wsRow,
		grpcUnaryRow,
		grpcStreamRow,
	}
}

// served makes n calls and counts the serving node of each; any error fails.
func served(t *testing.T, p *pipeline, row protoRow, n int) map[string]int {
	t.Helper()
	seen := map[string]int{}
	for i := 0; i < n; i++ {
		node, err := row.call(t, p)
		if err != nil {
			t.Fatalf("%s call %d: %v", row.name, i, err)
		}
		seen[node]++
	}
	return seen
}

// TestPipelineMatrix (#18): every client protocol through the production
// pipeline, against two fake nodes, for each behaviour of the issue's matrix.
func TestPipelineMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("pipeline matrix starts real servers")
	}
	for _, row := range matrixRows() {
		row := row
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			t.Run("happy path round-robins with metadata and metrics", func(t *testing.T) {
				t.Parallel()
				p := startPipeline(t)
				before := map[string]float64{}
				for _, n := range []string{p.a, p.b} {
					before[n] = p.metricValue(t, "sauron_node_requests_total", map[string]string{"node": n, "type": row.typ})
				}
				seen := served(t, p, row, 6)
				if seen[p.a] != 3 || seen[p.b] != 3 {
					t.Errorf("served by %v, want %s and %s 3 each (the node name comes from passed-through metadata)", seen, p.a, p.b)
				}
				for _, n := range []string{p.a, p.b} {
					d := p.metricValue(t, "sauron_node_requests_total", map[string]string{"node": n, "type": row.typ}) - before[n]
					if d != 3 {
						t.Errorf("sauron_node_requests_total{node=%q,type=%q} +%v, want +3", n, row.typ, d)
					}
				}
			})

			t.Run("backend error status reaches the client and keeps the node", func(t *testing.T) {
				t.Parallel()
				p := startPipeline(t)
				for i := 0; i < 4; i++ {
					row.backendError(t, p)
				}
				for _, n := range []string{p.a, p.b} {
					if got := p.s.circuitBreaker.GetErrorCount("pocket", n, row.breakerProto); got != 0 {
						t.Errorf("%s breaker count = %d after answers about the request, want 0", n, got)
					}
				}
				if seen := served(t, p, row, 4); seen[p.a] != 2 || seen[p.b] != 2 {
					t.Errorf("after backend errors served by %v, want both nodes still in rotation", seen)
				}
			})

			t.Run("unreachable node is excluded and the other serves", func(t *testing.T) {
				t.Parallel()
				p := startPipeline(t)
				p.nodes[p.b].kill()
				p.waitRoutable(t, p.b, false)
				if seen := served(t, p, row, 6); seen[p.a] != 6 {
					t.Errorf("with %s unreachable served by %v, want %s only", p.b, seen, p.a)
				}
			})

			t.Run("unhealthy excluded then recovered re-included", func(t *testing.T) {
				t.Parallel()
				p := startPipeline(t)
				p.nodes[p.b].node.SetFailed(true)
				p.waitRoutable(t, p.b, false)
				if seen := served(t, p, row, 6); seen[p.a] != 6 {
					t.Errorf("with %s failed served by %v, want %s only", p.b, seen, p.a)
				}
				p.nodes[p.b].node.SetFailed(false)
				p.waitRoutable(t, p.b, true)
				if seen := served(t, p, row, 6); seen[p.a] != 3 || seen[p.b] != 3 {
					t.Errorf("after %s recovered served by %v, want 3 each", p.b, seen)
				}
			})

			t.Run("client disconnect is not a node failure", func(t *testing.T) {
				t.Parallel()
				p := startPipeline(t)
				for i := 0; i < 4; i++ {
					row.clientGone(t, p)
				}
				time.Sleep(300 * time.Millisecond) // let the proxy record the outcome
				for _, n := range []string{p.a, p.b} {
					if got := p.s.circuitBreaker.GetErrorCount("pocket", n, row.breakerProto); got != 0 {
						t.Errorf("%s breaker count = %d after client disconnects, want 0", n, got)
					}
				}
				if seen := served(t, p, row, 4); seen[p.a] != 2 || seen[p.b] != 2 {
					t.Errorf("after client disconnects served by %v, want both nodes", seen)
				}
			})
		})
	}
}

// The status API reports both nodes with the height the health check read.
func TestPipelineStatusAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("pipeline starts real servers")
	}
	t.Parallel()
	p := startPipeline(t)
	code, _, body := httpGet(t, "http://"+p.status+"/pocket/status")
	if code != http.StatusOK {
		t.Fatalf("/pocket/status: HTTP %d %s", code, body)
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("decode /pocket/status: %v (%s)", err, body)
	}
	if !strings.Contains(body, "1000") {
		t.Errorf("/pocket/status = %s, want the nodes' height 1000", body)
	}
}
