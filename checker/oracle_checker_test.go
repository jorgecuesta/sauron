package checker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"sauron/adapter"
	"sauron/storage"

	"go.uber.org/zap"
)

func TestOracleChecker_CheckAll_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"result":"0x1312d00"}`))
	}))
	defer srv.Close()

	oracleStore := storage.NewOracleStore()
	engine := adapter.NewEngine(srv.Client())
	oc := NewOracleChecker(engine, oracleStore, zap.NewNop())

	oc.AddConfig(OracleConfig{
		Network: "ethereum",
		Oracles: []OracleEndpoint{
			{
				URL: srv.URL,
				Check: adapter.CheckConfig{
					Method:       "POST",
					URLPath:      "/",
					ResponsePath: ".result",
					Protocol:     "jsonrpc",
				},
			},
		},
	})

	oc.CheckAll(context.Background(), 5*time.Second)

	h, ok := oracleStore.Get("ethereum")
	if !ok {
		t.Fatal("expected oracle height to be stored")
	}
	if h != 20000000 { // 0x1312d00
		t.Fatalf("got height %d, want 20000000", h)
	}
}

func TestOracleChecker_CheckAll_MultipleOracles(t *testing.T) {
	t.Parallel()

	// Oracle 1 returns height 100, Oracle 2 returns height 200.
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"height": 100}`))
	}))
	defer srv1.Close()

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"height": 200}`))
	}))
	defer srv2.Close()

	oracleStore := storage.NewOracleStore()
	engine := adapter.NewEngine(&http.Client{Timeout: 2 * time.Second})
	oc := NewOracleChecker(engine, oracleStore, zap.NewNop())

	cfg := adapter.CheckConfig{
		Method:       "GET",
		URLPath:      "/",
		ResponsePath: ".height",
		Protocol:     "http",
	}

	oc.AddConfig(OracleConfig{
		Network: "pocket",
		Oracles: []OracleEndpoint{
			{URL: srv1.URL, Check: cfg},
			{URL: srv2.URL, Check: cfg},
		},
	})

	oc.CheckAll(context.Background(), 5*time.Second)

	// OracleStore keeps the highest — should be 200.
	h, ok := oracleStore.Get("pocket")
	if !ok {
		t.Fatal("expected oracle height")
	}
	if h != 200 {
		t.Fatalf("got height %d, want 200 (highest oracle)", h)
	}
}

func TestOracleChecker_CheckAll_MultipleNetworks(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		if r.URL.Path == "/pocket" {
			_, _ = w.Write([]byte(`{"height": 100}`))
		} else {
			_, _ = w.Write([]byte(`{"height": 20000000}`))
		}
	}))
	defer srv.Close()

	oracleStore := storage.NewOracleStore()
	engine := adapter.NewEngine(srv.Client())
	oc := NewOracleChecker(engine, oracleStore, zap.NewNop())

	oc.AddConfig(OracleConfig{
		Network: "pocket",
		Oracles: []OracleEndpoint{
			{URL: srv.URL, Check: adapter.CheckConfig{Method: "GET", URLPath: "/pocket", ResponsePath: ".height"}},
		},
	})
	oc.AddConfig(OracleConfig{
		Network: "ethereum",
		Oracles: []OracleEndpoint{
			{URL: srv.URL, Check: adapter.CheckConfig{Method: "GET", URLPath: "/eth", ResponsePath: ".height"}},
		},
	})

	oc.CheckAll(context.Background(), 5*time.Second)

	h1, _ := oracleStore.Get("pocket")
	h2, _ := oracleStore.Get("ethereum")
	if h1 != 100 {
		t.Fatalf("pocket: got %d, want 100", h1)
	}
	if h2 != 20000000 {
		t.Fatalf("ethereum: got %d, want 20000000", h2)
	}
}

func TestOracleChecker_CheckAll_OracleFails(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	oracleStore := storage.NewOracleStore()
	engine := adapter.NewEngine(srv.Client())
	oc := NewOracleChecker(engine, oracleStore, zap.NewNop())

	oc.AddConfig(OracleConfig{
		Network: "pocket",
		Oracles: []OracleEndpoint{
			{URL: srv.URL, Check: adapter.CheckConfig{Method: "GET", URLPath: "/", ResponsePath: ".height"}},
		},
	})

	oc.CheckAll(context.Background(), 5*time.Second)

	_, ok := oracleStore.Get("pocket")
	if ok {
		t.Fatal("expected no oracle height after failure")
	}
}

func TestOracleChecker_CheckAll_OracleReturnsZero(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"height": 0}`))
	}))
	defer srv.Close()

	oracleStore := storage.NewOracleStore()
	engine := adapter.NewEngine(srv.Client())
	oc := NewOracleChecker(engine, oracleStore, zap.NewNop())

	oc.AddConfig(OracleConfig{
		Network: "pocket",
		Oracles: []OracleEndpoint{
			{URL: srv.URL, Check: adapter.CheckConfig{Method: "GET", URLPath: "/", ResponsePath: ".height"}},
		},
	})

	oc.CheckAll(context.Background(), 5*time.Second)

	_, ok := oracleStore.Get("pocket")
	if ok {
		t.Fatal("expected no oracle height for zero result")
	}
}

func TestOracleChecker_CheckAll_OracleReturnsNegative(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"height": -1}`))
	}))
	defer srv.Close()

	oracleStore := storage.NewOracleStore()
	engine := adapter.NewEngine(srv.Client())
	oc := NewOracleChecker(engine, oracleStore, zap.NewNop())

	oc.AddConfig(OracleConfig{
		Network: "pocket",
		Oracles: []OracleEndpoint{
			{URL: srv.URL, Check: adapter.CheckConfig{Method: "GET", URLPath: "/", ResponsePath: ".height"}},
		},
	})

	oc.CheckAll(context.Background(), 5*time.Second)

	_, ok := oracleStore.Get("pocket")
	if ok {
		t.Fatal("expected no oracle height for negative result")
	}
}

func TestOracleChecker_CheckAll_PartialFailure(t *testing.T) {
	t.Parallel()

	// Oracle 1 fails, Oracle 2 succeeds.
	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := reqCount.Add(1)
		if n == 1 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"height": 42}`))
	}))
	defer srv.Close()

	oracleStore := storage.NewOracleStore()
	engine := adapter.NewEngine(srv.Client())
	oc := NewOracleChecker(engine, oracleStore, zap.NewNop())

	cfg := adapter.CheckConfig{Method: "GET", URLPath: "/", ResponsePath: ".height"}
	oc.AddConfig(OracleConfig{
		Network: "pocket",
		Oracles: []OracleEndpoint{
			{URL: srv.URL, Check: cfg},
			{URL: srv.URL, Check: cfg},
		},
	})

	oc.CheckAll(context.Background(), 5*time.Second)

	h, ok := oracleStore.Get("pocket")
	if !ok {
		t.Fatal("expected oracle height from second oracle")
	}
	if h != 42 {
		t.Fatalf("got %d, want 42", h)
	}
}

func TestOracleChecker_CheckAll_ContextCanceled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Second) // Slow oracle.
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"height": 100}`))
	}))
	defer srv.Close()

	oracleStore := storage.NewOracleStore()
	engine := adapter.NewEngine(srv.Client())
	oc := NewOracleChecker(engine, oracleStore, zap.NewNop())

	oc.AddConfig(OracleConfig{
		Network: "pocket",
		Oracles: []OracleEndpoint{
			{URL: srv.URL, Check: adapter.CheckConfig{Method: "GET", URLPath: "/", ResponsePath: ".height"}},
		},
	})

	// Very short timeout — should fail.
	oc.CheckAll(context.Background(), 50*time.Millisecond)

	_, ok := oracleStore.Get("pocket")
	if ok {
		t.Fatal("expected no oracle height after timeout")
	}
}

func TestOracleChecker_CheckAll_NoConfigs(t *testing.T) {
	t.Parallel()

	oracleStore := storage.NewOracleStore()
	engine := adapter.NewEngine(&http.Client{})
	oc := NewOracleChecker(engine, oracleStore, zap.NewNop())

	// No configs — should not panic.
	oc.CheckAll(context.Background(), 5*time.Second)

	all := oracleStore.All()
	if len(all) != 0 {
		t.Fatalf("expected empty oracle store, got %d entries", len(all))
	}
}

func TestOracleChecker_CheckAll_EmptyOracles(t *testing.T) {
	t.Parallel()

	oracleStore := storage.NewOracleStore()
	engine := adapter.NewEngine(&http.Client{})
	oc := NewOracleChecker(engine, oracleStore, zap.NewNop())

	oc.AddConfig(OracleConfig{
		Network: "pocket",
		Oracles: nil, // No oracles.
	})

	oc.CheckAll(context.Background(), 5*time.Second)

	_, ok := oracleStore.Get("pocket")
	if ok {
		t.Fatal("expected no oracle height with empty oracles list")
	}
}

func TestOracleChecker_Networks(t *testing.T) {
	t.Parallel()

	oc := NewOracleChecker(nil, nil, zap.NewNop())
	oc.AddConfig(OracleConfig{Network: "pocket"})
	oc.AddConfig(OracleConfig{Network: "ethereum"})

	networks := oc.Networks()
	if len(networks) != 2 {
		t.Fatalf("expected 2 networks, got %d", len(networks))
	}
}

func TestOracleChecker_Networks_Empty(t *testing.T) {
	t.Parallel()

	oc := NewOracleChecker(nil, nil, zap.NewNop())
	networks := oc.Networks()
	if len(networks) != 0 {
		t.Fatalf("expected 0 networks, got %d", len(networks))
	}
}

func TestOracleChecker_CheckAll_InvalidJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	oracleStore := storage.NewOracleStore()
	engine := adapter.NewEngine(srv.Client())
	oc := NewOracleChecker(engine, oracleStore, zap.NewNop())

	oc.AddConfig(OracleConfig{
		Network: "pocket",
		Oracles: []OracleEndpoint{
			{URL: srv.URL, Check: adapter.CheckConfig{Method: "GET", URLPath: "/", ResponsePath: ".height"}},
		},
	})

	oc.CheckAll(context.Background(), 5*time.Second)

	_, ok := oracleStore.Get("pocket")
	if ok {
		t.Fatal("expected no oracle height for invalid JSON")
	}
}

func TestOracleChecker_CheckAll_ConnectionRefused(t *testing.T) {
	t.Parallel()

	oracleStore := storage.NewOracleStore()
	engine := adapter.NewEngine(&http.Client{Timeout: 1 * time.Second})
	oc := NewOracleChecker(engine, oracleStore, zap.NewNop())

	oc.AddConfig(OracleConfig{
		Network: "pocket",
		Oracles: []OracleEndpoint{
			{URL: "http://127.0.0.1:0", Check: adapter.CheckConfig{Method: "GET", URLPath: "/", ResponsePath: ".height"}},
		},
	})

	oc.CheckAll(context.Background(), 2*time.Second)

	_, ok := oracleStore.Get("pocket")
	if ok {
		t.Fatal("expected no oracle height for unreachable host")
	}
}
