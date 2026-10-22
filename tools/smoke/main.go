// Command smoke checks every client path a Sauron deployment serves, so a
// deploy is verified against the service and not just against a diff.
//
// It covers what relay miners, gateways and indexers use: REST, RPC over GET
// and JSON-RPC POST, the CometBFT WebSocket subscription (an actual NewBlock
// event must arrive), a gRPC query, and the status port (/health, /ready,
// /metrics, /{network}/status). It exits non-zero if any check fails.
//
// Example (beta, from outside the cluster):
//
//	go run ./tools/smoke \
//	  -rest https://sauron-api.beta.infra.pocket.network \
//	  -rpc https://sauron-rpc.beta.infra.pocket.network \
//	  -grpc sauron-grpc.beta.infra.pocket.network:443 -grpc-tls \
//	  -status http://localhost:3000 -network pocket-lego-testnet
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

type result struct {
	name   string
	ok     bool
	detail string
}

func main() {
	rest := flag.String("rest", "", "REST (Cosmos API) base URL")
	rpc := flag.String("rpc", "", "RPC (CometBFT) base URL")
	grpcAddr := flag.String("grpc", "", "gRPC host:port")
	grpcTLS := flag.Bool("grpc-tls", false, "use TLS for gRPC")
	statusURL := flag.String("status", "", "status API base URL (optional)")
	network := flag.String("network", "", "network name for /{network}/status")
	wsTimeout := flag.Duration("ws-timeout", 2*time.Minute, "how long to wait for a NewBlock event")
	flag.Parse()

	var results []result
	client := &http.Client{Timeout: 15 * time.Second}

	if *rest != "" {
		results = append(results, checkREST(client, *rest))
	}
	if *rpc != "" {
		results = append(results, checkRPCGet(client, *rpc), checkRPCPost(client, *rpc), checkWebSocket(*rpc, *wsTimeout))
	}
	if *grpcAddr != "" {
		results = append(results, checkGRPC(*grpcAddr, *grpcTLS))
	}
	if *statusURL != "" {
		for _, path := range []string{"/health", "/ready", "/metrics"} {
			results = append(results, checkStatus200(client, *statusURL, path))
		}
		if *network != "" {
			results = append(results, checkNetworkStatus(client, *statusURL, *network))
		}
	}

	failed := 0
	for _, r := range results {
		mark := "PASS"
		if !r.ok {
			mark = "FAIL"
			failed++
		}
		fmt.Printf("%s  %-28s %s\n", mark, r.name, r.detail)
	}
	fmt.Printf("\n%d checks, %d failed\n", len(results), failed)
	if failed > 0 || len(results) == 0 {
		os.Exit(1)
	}
}

func get(client *http.Client, url string) (int, []byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

func checkREST(client *http.Client, base string) result {
	code, body, err := get(client, strings.TrimRight(base, "/")+"/cosmos/base/tendermint/v1beta1/blocks/latest")
	if err != nil || code != http.StatusOK {
		return result{"rest latest block", false, fmt.Sprintf("HTTP %d %v", code, err)}
	}
	var v struct {
		SDKBlock struct {
			Header struct {
				Height string `json:"height"`
			} `json:"header"`
		} `json:"sdk_block"`
	}
	if json.Unmarshal(body, &v) != nil || v.SDKBlock.Header.Height == "" {
		return result{"rest latest block", false, "no height in body"}
	}
	return result{"rest latest block", true, "height " + v.SDKBlock.Header.Height}
}

func rpcHeight(body []byte) string {
	var v struct {
		Result struct {
			SyncInfo struct {
				LatestBlockHeight string `json:"latest_block_height"`
			} `json:"sync_info"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return v.Result.SyncInfo.LatestBlockHeight
}

func checkRPCGet(client *http.Client, base string) result {
	code, body, err := get(client, strings.TrimRight(base, "/")+"/status")
	if err != nil || code != http.StatusOK || rpcHeight(body) == "" {
		return result{"rpc GET /status", false, fmt.Sprintf("HTTP %d %v", code, err)}
	}
	return result{"rpc GET /status", true, "height " + rpcHeight(body)}
}

func checkRPCPost(client *http.Client, base string) result {
	req := []byte(`{"jsonrpc":"2.0","id":1,"method":"status","params":{}}`)
	resp, err := client.Post(strings.TrimRight(base, "/"), "application/json", bytes.NewReader(req))
	if err != nil {
		return result{"rpc POST json-rpc status", false, err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || rpcHeight(body) == "" {
		return result{"rpc POST json-rpc status", false, fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return result{"rpc POST json-rpc status", true, "height " + rpcHeight(body)}
}

func checkWebSocket(base string, timeout time.Duration) result {
	const name = "rpc websocket NewBlock"
	u := strings.TrimRight(base, "/")
	u = strings.Replace(strings.Replace(u, "https://", "wss://", 1), "http://", "ws://", 1) + "/websocket"

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, u, nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		return result{name, false, fmt.Sprintf("upgrade failed (HTTP %d): %v", code, err)}
	}
	defer func() { _ = conn.Close() }()

	sub := `{"jsonrpc":"2.0","method":"subscribe","id":1,"params":{"query":"tm.event='NewBlock'"}}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(sub)); err != nil {
		return result{name, false, "subscribe: " + err.Error()}
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetReadDeadline(deadline)
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return result{name, false, "no NewBlock event: " + err.Error()}
		}
		var ev struct {
			Result struct {
				Data struct {
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
		if json.Unmarshal(msg, &ev) == nil && ev.Result.Data.Value.Block.Header.Height != "" {
			return result{name, true, "event at height " + ev.Result.Data.Value.Block.Header.Height}
		}
	}
}

// rawCodec sends and receives raw bytes, so a query needs no generated protos.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error)      { return *(v.(*[]byte)), nil }
func (rawCodec) Unmarshal(data []byte, v any) error { *(v.(*[]byte)) = data; return nil }
func (rawCodec) Name() string                       { return "proto" }

func checkGRPC(addr string, useTLS bool) result {
	const name = "grpc GetLatestBlock"
	creds := insecure.NewCredentials()
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{})
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds), grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})))
	if err != nil {
		return result{name, false, err.Error()}
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, resp := []byte{}, []byte{}
	var header metadata.MD
	err = conn.Invoke(ctx, "/cosmos.base.tendermint.v1beta1.Service/GetLatestBlock", &req, &resp, grpc.Header(&header))
	if err != nil {
		return result{name, false, err.Error()}
	}
	if len(resp) == 0 {
		return result{name, false, "empty response"}
	}
	detail := strconv.Itoa(len(resp)) + " bytes"
	if h := header.Get("x-cosmos-block-height"); len(h) > 0 {
		detail += ", x-cosmos-block-height " + h[0]
	} else {
		detail += ", no x-cosmos-block-height header"
	}
	return result{name, true, detail}
}

func checkStatus200(client *http.Client, base, path string) result {
	code, _, err := get(client, strings.TrimRight(base, "/")+path)
	return result{"status " + path, err == nil && code == http.StatusOK, fmt.Sprintf("HTTP %d", code)}
}

func checkNetworkStatus(client *http.Client, base, network string) result {
	name := "status /" + network + "/status"
	code, body, err := get(client, strings.TrimRight(base, "/")+"/"+network+"/status")
	if err != nil || code != http.StatusOK {
		return result{name, false, fmt.Sprintf("HTTP %d %v", code, err)}
	}
	var v struct {
		Height int64 `json:"height"`
	}
	if json.Unmarshal(body, &v) != nil || v.Height <= 0 {
		return result{name, false, "no height: " + string(body)}
	}
	return result{name, true, "height " + strconv.FormatInt(v.Height, 10)}
}
