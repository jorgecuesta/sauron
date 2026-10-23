// Command fakenode runs a deterministic fake Cosmos node for L3 tests; see
// internal/fakenode.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"sauron/internal/fakenode"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	name := envOr("FAKENODE_NAME", "fake")
	start, err := strconv.ParseInt(envOr("FAKENODE_START_HEIGHT", "1000"), 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad FAKENODE_START_HEIGHT:", err)
		os.Exit(2)
	}
	interval, err := time.ParseDuration(envOr("FAKENODE_BLOCK_INTERVAL", "2s"))
	if err != nil || interval <= 0 {
		fmt.Fprintln(os.Stderr, "bad FAKENODE_BLOCK_INTERVAL:", err)
		os.Exit(2)
	}

	var ls fakenode.Listeners
	for _, p := range []struct {
		dst  *net.Listener
		addr string
	}{{&ls.REST, ":1317"}, {&ls.RPC, ":26657"}, {&ls.GRPC, ":9090"}, {&ls.Admin, ":8000"}} {
		if *p.dst, err = net.Listen("tcp", p.addr); err != nil {
			fmt.Fprintln(os.Stderr, "listen:", err)
			os.Exit(1)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	fmt.Printf("fakenode %q start=%d interval=%s\n", name, start, interval)
	if err := fakenode.Run(ctx, fakenode.NewNode(name, start, interval), ls); err != nil {
		fmt.Fprintln(os.Stderr, "fakenode:", err)
		os.Exit(1)
	}
}
