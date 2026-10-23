// Package fakenode is a deterministic fake Cosmos/CometBFT node used as a
// backend for Sauron's L3 docker tests. Its height, health and freeze state
// are fully controlled via env config and an admin API.
package fakenode

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sauron/internal/grpcutil"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Node is the shared state behind every protocol served by the fake node.
type Node struct {
	name     string
	interval time.Duration
	height   atomic.Int64
	failed   atomic.Bool
	frozen   atomic.Bool

	mu     sync.Mutex
	wakeup chan struct{} // closed and replaced on every height/fail change
}

// NewNode builds a node starting at startHeight, ticking every interval.
func NewNode(name string, startHeight int64, interval time.Duration) *Node {
	n := &Node{name: name, interval: interval, wakeup: make(chan struct{})}
	n.height.Store(startHeight)
	return n
}

// Height returns the current block height.
func (n *Node) Height() int64 { return n.height.Load() }

// SetFailed enters or leaves the failed state.
func (n *Node) SetFailed(v bool) { n.failed.Store(v); n.notify() }

// SetFrozen stops or resumes height growth.
func (n *Node) SetFrozen(v bool) { n.frozen.Store(v) }

func (n *Node) notify() {
	n.mu.Lock()
	close(n.wakeup)
	n.wakeup = make(chan struct{})
	n.mu.Unlock()
}

func (n *Node) wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.wakeup
}

// RunTicker increments the height every interval until ctx is done.
func (n *Node) RunTicker(ctx context.Context) {
	t := time.NewTicker(n.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !n.frozen.Load() {
				n.height.Add(1)
				n.notify()
			}
		}
	}
}

// guard tags every response with X-Fake-Node and rejects with 503 when failed.
func (n *Node) guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Fake-Node", n.name)
		if n.failed.Load() {
			http.Error(w, "node failed", http.StatusServiceUnavailable)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// RESTHandler serves the Cosmos REST API subset.
func (n *Node) RESTHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/cosmos/base/tendermint/v1beta1/blocks/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"sdk_block": map[string]any{"header": map[string]any{
			"height": strconv.FormatInt(n.Height(), 10),
		}}})
	})
	// /slow answers only when the client gives up (client-disconnect tests).
	mux.HandleFunc("GET /slow", func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	return n.guard(mux)
}

func (n *Node) statusResult(id json.RawMessage) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
		"sync_info": map[string]any{"latest_block_height": strconv.FormatInt(n.Height(), 10)},
	}}
}

// abciInfoResult is /abci_info: the app-committed height sauron's Cosmos
// height check reads.
func (n *Node) abciInfoResult(id json.RawMessage) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
		"response": map[string]any{"data": "fakenode", "last_block_height": strconv.FormatInt(n.Height(), 10)},
	}}
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// RPCHandler serves the CometBFT RPC subset (HTTP, JSON-RPC, WebSocket).
func (n *Node) RPCHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, n.statusResult(json.RawMessage("-1")))
	})
	mux.HandleFunc("GET /abci_info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, n.abciInfoResult(json.RawMessage("-1")))
	})
	mux.HandleFunc("POST /{$}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(req.ID) == 0 {
			req.ID = json.RawMessage("null")
		}
		switch req.Method {
		case "status":
			writeJSON(w, n.statusResult(req.ID))
			return
		case "abci_info":
			writeJSON(w, n.abciInfoResult(req.ID))
			return
		}
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": -32601, "message": "Method not found"}})
	})
	mux.HandleFunc("GET /websocket", n.serveWS)
	// /slow answers only when the client gives up (client-disconnect tests).
	mux.HandleFunc("GET /slow", func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	return n.guard(mux)
}

func (n *Node) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, http.Header{"X-Fake-Node": {n.name}})
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	subs := make(chan json.RawMessage, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			if msg.Method == "subscribe" {
				if len(msg.ID) == 0 {
					msg.ID = json.RawMessage("null")
				}
				select {
				case subs <- msg.ID:
				default:
				}
			}
		}
	}()

	var subID json.RawMessage
	last := n.Height()
	for {
		wake := n.wait()
		if n.failed.Load() {
			return
		}
		if subID != nil {
			if h := n.Height(); h != last {
				last = h
				if conn.WriteJSON(newBlockEvent(subID, h)) != nil {
					return
				}
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-readDone:
			return
		case id := <-subs:
			subID = id
			last = n.Height()
			ack := map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}}
			if conn.WriteJSON(ack) != nil {
				return
			}
		case <-wake:
		}
	}
}

func newBlockEvent(id json.RawMessage, h int64) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
		"query": "tm.event='NewBlock'",
		"data": map[string]any{
			"type": "tendermint/event/NewBlock",
			"value": map[string]any{"block": map[string]any{"header": map[string]any{
				"height": strconv.FormatInt(h, 10),
			}}},
		},
	}}
}

// AdminHandler serves control endpoints; it is never guarded by failed state.
func (n *Node) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	set := func(path string, f func()) {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, _ *http.Request) {
			f()
			writeJSON(w, n.state())
		})
	}
	set("/admin/fail", func() { n.SetFailed(true) })
	set("/admin/recover", func() { n.SetFailed(false) })
	set("/admin/freeze", func() { n.SetFrozen(true) })
	set("/admin/unfreeze", func() { n.SetFrozen(false) })
	mux.HandleFunc("GET /admin/state", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, n.state()) })
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Fake-Node", n.name)
		mux.ServeHTTP(w, r)
	})
}

func (n *Node) state() map[string]any {
	return map[string]any{"name": n.name, "height": n.Height(), "failed": n.failed.Load(), "frozen": n.frozen.Load()}
}

// NewGRPCServer returns a server answering every method with a fixed payload.
func (n *Node) NewGRPCServer() *grpc.Server {
	return grpc.NewServer(
		grpc.ForceServerCodec(grpcutil.BytesCodec{}),
		grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
			if n.failed.Load() {
				return status.Error(codes.Unavailable, "node failed")
			}
			var in []byte
			if err := stream.RecvMsg(&in); err != nil {
				return err
			}
			h := n.Height()
			if err := stream.SetHeader(metadata.Pairs(
				"x-cosmos-block-height", strconv.FormatInt(h, 10),
				"x-fake-node", n.name,
			)); err != nil {
				return err
			}
			// The method name selects a behaviour for pipeline tests.
			method, _ := grpc.MethodFromServerStream(stream)
			switch {
			case strings.HasSuffix(method, "/NotFound"):
				return status.Error(codes.NotFound, "not found")
			case strings.HasSuffix(method, "/Slow"):
				<-stream.Context().Done()
				return stream.Context().Err()
			}
			out := make([]byte, 8)
			for i := range out {
				out[i] = byte(h >> (8 * (7 - i)))
			}
			sends := 1
			if strings.HasSuffix(method, "/Stream") {
				sends = 3
			}
			for i := 0; i < sends; i++ {
				if err := stream.SendMsg(&out); err != nil {
					return err
				}
			}
			return nil
		}),
	)
}

// Listeners holds one listener per served protocol.
type Listeners struct{ REST, RPC, GRPC, Admin net.Listener }

// Run serves all protocols until ctx is cancelled, then shuts down gracefully.
func Run(ctx context.Context, n *Node, ls Listeners) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); n.RunTicker(ctx) }()

	errCh := make(chan error, 4)
	var httpSrvs []*http.Server
	for _, s := range []struct {
		ln net.Listener
		h  http.Handler
	}{{ls.REST, n.RESTHandler()}, {ls.RPC, n.RPCHandler()}, {ls.Admin, n.AdminHandler()}} {
		srv := &http.Server{Handler: s.h, ReadHeaderTimeout: 5 * time.Second,
			BaseContext: func(net.Listener) context.Context { return ctx }}
		httpSrvs = append(httpSrvs, srv)
		go func(ln net.Listener) {
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(s.ln)
	}
	gs := n.NewGRPCServer()
	go func() {
		if err := gs.Serve(ls.GRPC); err != nil {
			errCh <- err
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
	}

	cancel() // unblocks hijacked WebSocket handlers
	sctx, scancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer scancel()
	for _, s := range httpSrvs {
		_ = s.Shutdown(sctx)
	}
	stopped := make(chan struct{})
	go func() { gs.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-sctx.Done():
		gs.Stop()
	}
	wg.Wait()
	return runErr
}
