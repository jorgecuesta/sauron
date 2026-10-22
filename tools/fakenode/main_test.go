package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type env struct {
	n                    *Node
	rest, rpc, grpc, adm string
	cancel               context.CancelFunc
	done                 chan error
}

func startEnv(t *testing.T, interval time.Duration) *env {
	t.Helper()
	var ls Listeners
	var addrs [4]string
	for i, dst := range []*net.Listener{&ls.REST, &ls.RPC, &ls.GRPC, &ls.Admin} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		*dst = ln
		addrs[i] = ln.Addr().String()
	}
	n := NewNode("n1", 1000, interval)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, n, ls) }()
	e := &env{n: n, rest: addrs[0], rpc: addrs[1], grpc: addrs[2], adm: addrs[3], cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return e
}

func do(t *testing.T, method, url, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

const restPath = "/cosmos/base/tendermint/v1beta1/blocks/latest"

func TestRESTLatestBlock(t *testing.T) {
	e := startEnv(t, time.Hour)
	resp, body := do(t, "GET", "http://"+e.rest+restPath, "")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var got struct {
		SDKBlock struct {
			Header struct {
				Height string `json:"height"`
			} `json:"header"`
		} `json:"sdk_block"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.SDKBlock.Header.Height != "1000" {
		t.Errorf("height = %q, want 1000", got.SDKBlock.Header.Height)
	}
	if h := resp.Header.Get("X-Fake-Node"); h != "n1" {
		t.Errorf("X-Fake-Node = %q", h)
	}
}

func TestRESTUnknownPath404(t *testing.T) {
	e := startEnv(t, time.Hour)
	resp, _ := do(t, "GET", "http://"+e.rest+"/nope", "")
	if resp.StatusCode != 404 {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
	if resp.Header.Get("X-Fake-Node") != "n1" {
		t.Error("missing X-Fake-Node on 404")
	}
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  struct {
		SyncInfo struct {
			Height string `json:"latest_block_height"`
		} `json:"sync_info"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeRPC(t *testing.T, body string) rpcResp {
	t.Helper()
	var r rpcResp
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return r
}

func TestRPCStatusGET(t *testing.T) {
	e := startEnv(t, time.Hour)
	resp, body := do(t, "GET", "http://"+e.rpc+"/status", "")
	r := decodeRPC(t, body)
	if resp.StatusCode != 200 || r.JSONRPC != "2.0" || string(r.ID) != "-1" || r.Result.SyncInfo.Height != "1000" {
		t.Errorf("status=%d body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Fake-Node") != "n1" {
		t.Error("missing X-Fake-Node")
	}
}

func TestRPCStatusPOSTEchoesID(t *testing.T) {
	e := startEnv(t, time.Hour)
	for _, id := range []string{`7`, `"abc"`} {
		resp, body := do(t, "POST", "http://"+e.rpc+"/", `{"jsonrpc":"2.0","id":`+id+`,"method":"status","params":{}}`)
		r := decodeRPC(t, body)
		if resp.StatusCode != 200 || string(r.ID) != id || r.Result.SyncInfo.Height != "1000" {
			t.Errorf("id=%s status=%d body=%s", id, resp.StatusCode, body)
		}
		if resp.Header.Get("X-Fake-Node") != "n1" {
			t.Error("missing X-Fake-Node")
		}
	}
}

func TestRPCUnknownMethod(t *testing.T) {
	e := startEnv(t, time.Hour)
	resp, body := do(t, "POST", "http://"+e.rpc+"/", `{"jsonrpc":"2.0","id":3,"method":"abci_info"}`)
	r := decodeRPC(t, body)
	if resp.StatusCode != 200 || r.Error == nil || r.Error.Code != -32601 || r.Error.Message != "Method not found" || string(r.ID) != "3" {
		t.Errorf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestRPCBadBody(t *testing.T) {
	e := startEnv(t, time.Hour)
	resp, _ := do(t, "POST", "http://"+e.rpc+"/", `not json`)
	if resp.StatusCode != 400 {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

func TestAdminEndpointsHeaderAndState(t *testing.T) {
	e := startEnv(t, time.Hour)
	for _, c := range []struct{ method, path string }{
		{"GET", "/healthz"}, {"GET", "/admin/state"}, {"POST", "/admin/freeze"},
		{"POST", "/admin/unfreeze"}, {"POST", "/admin/fail"}, {"POST", "/admin/recover"},
	} {
		resp, _ := do(t, c.method, "http://"+e.adm+c.path, "")
		if resp.StatusCode != 200 || resp.Header.Get("X-Fake-Node") != "n1" {
			t.Errorf("%s %s: status=%d header=%q", c.method, c.path, resp.StatusCode, resp.Header.Get("X-Fake-Node"))
		}
	}
	do(t, "POST", "http://"+e.adm+"/admin/fail", "")
	_, body := do(t, "GET", "http://"+e.adm+"/admin/state", "")
	var st struct {
		Name           string `json:"name"`
		Height         int64  `json:"height"`
		Failed, Frozen bool
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Name != "n1" || st.Height != 1000 || !st.Failed || st.Frozen {
		t.Errorf("state = %+v", st)
	}
	_, body = do(t, "POST", "http://"+e.adm+"/admin/freeze", "")
	if !strings.Contains(body, `"frozen":true`) {
		t.Errorf("freeze body: %s", body)
	}
}

func TestHeightIncrementsAndFreeze(t *testing.T) {
	e := startEnv(t, 20*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for e.n.Height() < 1003 {
		if time.Now().After(deadline) {
			t.Fatalf("height stuck at %d", e.n.Height())
		}
		time.Sleep(5 * time.Millisecond)
	}
	do(t, "POST", "http://"+e.adm+"/admin/freeze", "")
	time.Sleep(40 * time.Millisecond) // let an in-flight tick land
	h := e.n.Height()
	time.Sleep(200 * time.Millisecond)
	if got := e.n.Height(); got != h {
		t.Errorf("height moved while frozen: %d -> %d", h, got)
	}
	_, body := do(t, "GET", "http://"+e.rest+restPath, "")
	if !strings.Contains(body, `"`+strconv.FormatInt(h, 10)+`"`) {
		t.Errorf("REST height mismatch: %s vs %d", body, h)
	}
	do(t, "POST", "http://"+e.adm+"/admin/unfreeze", "")
	deadline = time.Now().Add(2 * time.Second)
	for e.n.Height() <= h {
		if time.Now().After(deadline) {
			t.Fatal("height did not resume after unfreeze")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func dialWS(t *testing.T, e *env) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	return websocket.DefaultDialer.Dial("ws://"+e.rpc+"/websocket", nil)
}

func TestWebSocketSubscribe(t *testing.T) {
	e := startEnv(t, 20*time.Millisecond)
	c, _, err := dialWS(t, e)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": 42, "method": "subscribe", "params": map[string]string{"query": "tm.event='NewBlock'"}}); err != nil {
		t.Fatal(err)
	}
	var ack struct {
		ID     json.RawMessage `json:"id"`
		Result map[string]any  `json:"result"`
	}
	if err := c.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	if string(ack.ID) != "42" || ack.Result == nil || len(ack.Result) != 0 {
		t.Errorf("ack = %+v", ack)
	}
	var heights []int64
	for len(heights) < 2 {
		var ev struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				Query string `json:"query"`
				Data  struct {
					Type  string `json:"type"`
					Value struct {
						Block struct {
							Header struct {
								Height string `json:"height"`
							} `json:"header"`
						} `json:"block"`
					} `json:"value"`
				} `json:"data"`
			} `json:"result"`
		}
		if err := c.ReadJSON(&ev); err != nil {
			t.Fatal(err)
		}
		if string(ev.ID) != "42" || ev.Result.Query != "tm.event='NewBlock'" || ev.Result.Data.Type != "tendermint/event/NewBlock" {
			t.Fatalf("bad event %+v", ev)
		}
		h, err := strconv.ParseInt(ev.Result.Data.Value.Block.Header.Height, 10, 64)
		if err != nil || h <= 1000 {
			t.Fatalf("bad height %q", ev.Result.Data.Value.Block.Header.Height)
		}
		heights = append(heights, h)
	}
	if heights[1] <= heights[0] {
		t.Errorf("heights not increasing: %v", heights)
	}
}

func TestWebSocketClientDisconnectDoesNotBreakServer(t *testing.T) {
	e := startEnv(t, 10*time.Millisecond)
	for i := 0; i < 5; i++ {
		c, _, err := dialWS(t, e)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.WriteJSON(map[string]any{"id": i, "method": "subscribe"})
		_ = c.Close()
	}
	time.Sleep(100 * time.Millisecond)
	resp, _ := do(t, "GET", "http://"+e.rpc+"/status", "")
	if resp.StatusCode != 200 {
		t.Errorf("status %d", resp.StatusCode)
	}
}

type rawClientCodec struct{}

func (rawClientCodec) Marshal(v any) ([]byte, error) { return v.([]byte), nil }
func (rawClientCodec) Unmarshal(d []byte, v any) error {
	*(v.(*[]byte)) = append([]byte(nil), d...)
	return nil
}
func (rawClientCodec) Name() string { return "proto" }

func grpcCall(t *testing.T, e *env) ([]byte, metadata.MD, error) {
	t.Helper()
	cc, err := grpc.NewClient(e.grpc, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawClientCodec{})))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cc.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out []byte
	var hdr metadata.MD
	err = cc.Invoke(ctx, "/pocket.session.Query/GetSession", []byte("req"), &out, grpc.Header(&hdr))
	return out, hdr, err
}

func TestGRPCAnyMethod(t *testing.T) {
	e := startEnv(t, time.Hour)
	out, hdr, err := grpcCall(t, e)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 8 {
		t.Errorf("payload len %d", len(out))
	}
	if got := hdr.Get("x-cosmos-block-height"); len(got) != 1 || got[0] != "1000" {
		t.Errorf("x-cosmos-block-height = %v", got)
	}
	if got := hdr.Get("x-fake-node"); len(got) != 1 || got[0] != "n1" {
		t.Errorf("x-fake-node = %v", got)
	}
}

func TestFailedStateAllProtocols(t *testing.T) {
	e := startEnv(t, time.Hour)
	// Open a WS before failing so we can observe it being closed.
	c, _, err := dialWS(t, e)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	do(t, "POST", "http://"+e.adm+"/admin/fail", "")

	for _, u := range []string{"http://" + e.rest + restPath, "http://" + e.rpc + "/status"} {
		resp, body := do(t, "GET", u, "")
		if resp.StatusCode != 503 || !strings.Contains(body, "node failed") || resp.Header.Get("X-Fake-Node") != "n1" {
			t.Errorf("%s: status=%d body=%q", u, resp.StatusCode, body)
		}
	}
	resp, _ := do(t, "POST", "http://"+e.rpc+"/", `{"id":1,"method":"status"}`)
	if resp.StatusCode != 503 {
		t.Errorf("POST status %d, want 503", resp.StatusCode)
	}

	if _, wsResp, err := dialWS(t, e); err == nil {
		t.Error("websocket upgrade should be refused")
	} else if wsResp == nil || wsResp.StatusCode != 503 {
		t.Errorf("ws refusal resp = %v", wsResp)
	}

	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Error("existing websocket should be closed")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Error("existing websocket not closed before timeout")
	}

	if _, _, err := grpcCall(t, e); status.Code(err) != codes.Unavailable {
		t.Errorf("grpc err = %v, want Unavailable", err)
	}

	resp, _ = do(t, "GET", "http://"+e.adm+"/healthz", "")
	if resp.StatusCode != 200 {
		t.Errorf("healthz %d while failed", resp.StatusCode)
	}
	_, body := do(t, "GET", "http://"+e.adm+"/admin/state", "")
	if !strings.Contains(body, `"failed":true`) {
		t.Errorf("state: %s", body)
	}
}

func TestRecoverRestoresEverything(t *testing.T) {
	e := startEnv(t, time.Hour)
	do(t, "POST", "http://"+e.adm+"/admin/fail", "")
	do(t, "POST", "http://"+e.adm+"/admin/recover", "")

	if resp, _ := do(t, "GET", "http://"+e.rest+restPath, ""); resp.StatusCode != 200 {
		t.Errorf("rest %d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", "http://"+e.rpc+"/status", ""); resp.StatusCode != 200 {
		t.Errorf("rpc %d", resp.StatusCode)
	}
	c, _, err := dialWS(t, e)
	if err != nil {
		t.Fatalf("ws after recover: %v", err)
	}
	_ = c.Close()
	if _, _, err := grpcCall(t, e); err != nil {
		t.Errorf("grpc after recover: %v", err)
	}
}

func TestConcurrentAccess(t *testing.T) {
	e := startEnv(t, time.Millisecond)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = e.n.Height()
					if resp, err := http.Get("http://" + e.rest + restPath); err == nil {
						_, _ = io.Copy(io.Discard, resp.Body)
						_ = resp.Body.Close()
					}
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			e.n.SetFailed(i%2 == 0)
			e.n.SetFrozen(i%3 == 0)
		}
		e.n.SetFailed(false)
		e.n.SetFrozen(false)
	}()
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestRunReturnsOnCancel(t *testing.T) {
	e := startEnv(t, time.Hour)
	c, _, err := dialWS(t, e)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	e.cancel()
	select {
	case err := <-e.done:
		if err != nil {
			t.Errorf("Run err = %v", err)
		}
		e.done <- nil // let cleanup drain
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return with open websocket")
	}
}
