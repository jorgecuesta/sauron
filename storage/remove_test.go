package storage

import (
	"sort"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// keepNodes returns a keep predicate for the given "network/node" pairs.
func keepNodes(pairs ...string) func(network, node string) bool {
	set := map[string]bool{}
	for _, p := range pairs {
		set[p] = true
	}
	return func(network, node string) bool { return set[network+"/"+node] }
}

func refsString(refs []NodeRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Network+"/"+r.Node)
	}
	sort.Strings(out)
	return out
}

// #20: a node removed from the config is dropped from the height store (all
// its endpoint types), reported once, and nothing else is touched.
func TestHeightStore_RemoveNodes(t *testing.T) {
	t.Parallel()
	s := NewHeightStore()
	s.Update("pocket", "seed-one", "rpc", 100, time.Millisecond, "internal")
	s.Update("pocket", "seed-one", "rest", 100, time.Millisecond, "internal")
	s.Update("pocket", "seed-two", "rpc", 101, time.Millisecond, "internal")
	s.Update("beta", "seed-one", "rpc", 50, time.Millisecond, "internal") // same name, other network
	s.Update("pocket", "node:with:colons", "rpc", 99, time.Millisecond, "internal")

	removed := s.RemoveNodes(keepNodes("pocket/seed-two", "beta/seed-one"))

	if got := refsString(removed); len(got) != 2 || got[0] != "pocket/node:with:colons" || got[1] != "pocket/seed-one" {
		t.Errorf("removed = %v, want pocket/node:with:colons and pocket/seed-one, once each", got)
	}
	if _, ok := s.GetByNetwork("pocket", "rpc")["seed-one"]; ok {
		t.Error("pocket/seed-one rpc still stored")
	}
	if _, ok := s.GetByNetwork("pocket", "rest")["seed-one"]; ok {
		t.Error("pocket/seed-one rest still stored")
	}
	if _, ok := s.GetByNetwork("pocket", "rpc")["node:with:colons"]; ok {
		t.Error("node with colons in its name still stored")
	}
	if m := s.GetByNetwork("pocket", "rpc")["seed-two"]; m == nil || m.Height != 101 {
		t.Errorf("kept pocket/seed-two = %+v, want height 101", m)
	}
	if m := s.GetByNetwork("beta", "rpc")["seed-one"]; m == nil || m.Height != 50 {
		t.Errorf("beta/seed-one with the same name as a removed node = %+v, want kept at 50", m)
	}
	if again := s.RemoveNodes(keepNodes("pocket/seed-two", "beta/seed-one")); len(again) != 0 {
		t.Errorf("second removal reported %v, want nothing", refsString(again))
	}
}

func TestHeightStore_RemoveNodes_Empty(t *testing.T) {
	t.Parallel()
	if removed := NewHeightStore().RemoveNodes(func(string, string) bool { return false }); len(removed) != 0 {
		t.Errorf("empty store removed %v", removed)
	}
}

func TestHealthStore_RemoveNodes(t *testing.T) {
	t.Parallel()
	s := NewHealthStore()
	s.SetHealthy("pocket", "seed-one", "rpc")
	s.SetUnhealthy("pocket", "seed-one", "grpc", "down")
	s.SetHealthy("pocket", "seed-two", "rpc")
	s.SetHealthy("beta", "seed-one", "rpc")

	if n := s.RemoveNodes(keepNodes("pocket/seed-two", "beta/seed-one")); n != 2 {
		t.Errorf("removed %d entries, want 2 (seed-one rpc and grpc)", n)
	}
	if s.GetHealth("pocket", "seed-one", "rpc") != nil || s.GetHealth("pocket", "seed-one", "grpc") != nil {
		t.Error("removed node's health still stored")
	}
	if !s.IsHealthy("pocket", "seed-two", "rpc") || !s.IsHealthy("beta", "seed-one", "rpc") {
		t.Error("kept nodes lost their health")
	}
}

func TestArchivalStore_RemoveNodes(t *testing.T) {
	t.Parallel()
	s := NewArchivalStore()
	s.SetArchival("pocket", "seed-one")
	s.SetNotArchival("pocket", "seed-two")

	if n := s.RemoveNodes(keepNodes("pocket/seed-two")); n != 1 {
		t.Errorf("removed %d, want 1", n)
	}
	if s.GetStatus("pocket", "seed-one") != nil {
		t.Error("removed node's archival status still stored")
	}
	if st := s.GetStatus("pocket", "seed-two"); st == nil || st.IsArchival {
		t.Errorf("kept node's status = %+v, want not archival", st)
	}
}

// Removal runs while checks write: no race, and kept nodes stay.
func TestStores_RemoveNodes_Concurrent(t *testing.T) {
	t.Parallel()
	heights, health := NewHeightStore(), NewHealthStore()
	keep := keepNodes("pocket/keep")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				heights.Update("pocket", "keep", "rpc", int64(j+1), time.Millisecond, "internal")
				heights.Update("pocket", "gone", "rpc", int64(j+1), time.Millisecond, "internal")
				health.SetHealthy("pocket", "keep", "rpc")
				health.SetHealthy("pocket", "gone", "rpc")
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				heights.RemoveNodes(keep)
				health.RemoveNodes(keep)
			}
		}()
	}
	wg.Wait()
	if _, ok := heights.GetByNetwork("pocket", "rpc")["keep"]; !ok {
		t.Error("kept node lost under concurrent removal")
	}
}

// A removed external, or a ring removed from an external, drops its
// endpoints; the removed copies are returned for connection cleanup.
func TestExternalEndpointStore_RemoveExternals(t *testing.T) {
	t.Parallel()
	s := NewExternalEndpointStore(zap.NewNop())
	s.StoreAdvertised("partner", "https://ring-a", "pocket", "grpc", "a.example.com:443")
	s.StoreAdvertised("partner", "https://ring-b", "pocket", "rest", "https://b.example.com")
	s.StoreAdvertised("gone", "https://ring-g", "pocket", "rest", "https://g.example.com")
	s.MarkValidated("gone", "https://ring-g", "pocket", "rest", "https://g.example.com", 100, time.Millisecond)

	keep := func(external, ring string) bool { return external == "partner" && ring == "https://ring-a" }
	removed := s.RemoveExternals(keep)

	urls := []string{}
	for _, ep := range removed {
		urls = append(urls, ep.URL)
	}
	sort.Strings(urls)
	if len(urls) != 2 || urls[0] != "https://b.example.com" || urls[1] != "https://g.example.com" {
		t.Errorf("removed = %v, want b and g", urls)
	}
	if len(s.GetValidatedEndpoints("pocket", "rest")) != 0 {
		t.Error("removed external still routable")
	}
	if eps := s.EndpointsForRing("partner", "https://ring-a", "pocket"); len(eps) != 1 {
		t.Errorf("kept ring endpoints = %d, want 1", len(eps))
	}
}
