package storage

import (
	"sync"
	"testing"
)

func TestOracleStore_Update_And_Get(t *testing.T) {
	t.Parallel()
	s := NewOracleStore()

	s.Update("pocket", 100, "https://oracle1.example.com")

	h, ok := s.Get("pocket")
	if !ok {
		t.Fatal("expected oracle height to exist")
	}
	if h != 100 {
		t.Fatalf("got %d, want 100", h)
	}
}

func TestOracleStore_Update_HigherReplacesLower(t *testing.T) {
	t.Parallel()
	s := NewOracleStore()

	s.Update("pocket", 100, "oracle-a")
	s.Update("pocket", 200, "oracle-b")

	h, _ := s.Get("pocket")
	if h != 200 {
		t.Fatalf("got %d, want 200", h)
	}

	full := s.GetFull("pocket")
	if full.Source != "oracle-b" {
		t.Fatalf("source: got %q, want oracle-b", full.Source)
	}
}

func TestOracleStore_Update_LowerIgnored(t *testing.T) {
	t.Parallel()
	s := NewOracleStore()

	s.Update("pocket", 200, "oracle-a")
	s.Update("pocket", 100, "oracle-b") // Lower — should be ignored.

	h, _ := s.Get("pocket")
	if h != 200 {
		t.Fatalf("got %d, want 200 (lower update should be ignored)", h)
	}

	full := s.GetFull("pocket")
	if full.Source != "oracle-a" {
		t.Fatalf("source should stay oracle-a, got %q", full.Source)
	}
}

func TestOracleStore_Update_EqualIgnored(t *testing.T) {
	t.Parallel()
	s := NewOracleStore()

	s.Update("pocket", 100, "oracle-a")
	s.Update("pocket", 100, "oracle-b") // Equal — should be ignored.

	full := s.GetFull("pocket")
	if full.Source != "oracle-a" {
		t.Fatalf("source should stay oracle-a for equal height, got %q", full.Source)
	}
}

func TestOracleStore_Get_NotFound(t *testing.T) {
	t.Parallel()
	s := NewOracleStore()

	h, ok := s.Get("nonexistent")
	if ok {
		t.Fatal("expected ok=false for missing network")
	}
	if h != 0 {
		t.Fatalf("expected 0, got %d", h)
	}
}

func TestOracleStore_GetFull_NotFound(t *testing.T) {
	t.Parallel()
	s := NewOracleStore()

	if s.GetFull("nonexistent") != nil {
		t.Fatal("expected nil for missing network")
	}
}

func TestOracleStore_GetFull_ReturnsCopy(t *testing.T) {
	t.Parallel()
	s := NewOracleStore()

	s.Update("pocket", 100, "oracle-a")
	full := s.GetFull("pocket")

	// Mutate the copy.
	full.Height = 999
	full.Source = "mutated"

	// Original should be unaffected.
	h, _ := s.Get("pocket")
	if h != 100 {
		t.Fatalf("original height mutated: got %d", h)
	}
}

func TestOracleStore_GetFull_HasTimestamp(t *testing.T) {
	t.Parallel()
	s := NewOracleStore()

	s.Update("pocket", 100, "oracle-a")
	full := s.GetFull("pocket")

	if full.Timestamp.IsZero() {
		t.Fatal("expected non-zero timestamp")
	}
}

func TestOracleStore_IndependentNetworks(t *testing.T) {
	t.Parallel()
	s := NewOracleStore()

	s.Update("pocket", 100, "oracle-pocket")
	s.Update("ethereum", 20000000, "oracle-eth")

	h1, _ := s.Get("pocket")
	h2, _ := s.Get("ethereum")

	if h1 != 100 {
		t.Fatalf("pocket: got %d, want 100", h1)
	}
	if h2 != 20000000 {
		t.Fatalf("ethereum: got %d, want 20000000", h2)
	}
}

func TestOracleStore_All(t *testing.T) {
	t.Parallel()
	s := NewOracleStore()

	s.Update("pocket", 100, "oracle-a")
	s.Update("ethereum", 200, "oracle-b")

	all := s.All()
	if len(all) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(all))
	}
	if all["pocket"].Height != 100 {
		t.Fatalf("pocket: got %d", all["pocket"].Height)
	}
	if all["ethereum"].Height != 200 {
		t.Fatalf("ethereum: got %d", all["ethereum"].Height)
	}
}

func TestOracleStore_All_Empty(t *testing.T) {
	t.Parallel()
	s := NewOracleStore()

	all := s.All()
	if len(all) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(all))
	}
}

func TestOracleStore_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	s := NewOracleStore()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		i := i
		wg.Add(3)
		go func() {
			defer wg.Done()
			s.Update("pocket", int64(i), "oracle")
		}()
		go func() {
			defer wg.Done()
			_, _ = s.Get("pocket")
		}()
		go func() {
			defer wg.Done()
			_ = s.All()
		}()
	}
	wg.Wait()

	// Final height should be somewhere between 0 and 99.
	// Due to the "only higher" rule and concurrency, exact value is non-deterministic.
	h, ok := s.Get("pocket")
	if !ok {
		t.Fatal("expected oracle height after concurrent updates")
	}
	if h < 0 || h > 99 {
		t.Fatalf("unexpected height %d", h)
	}
}

func TestOracleStore_ZeroHeight(t *testing.T) {
	t.Parallel()
	s := NewOracleStore()

	s.Update("pocket", 0, "oracle-a")

	h, ok := s.Get("pocket")
	if !ok {
		t.Fatal("expected oracle height to exist even at 0")
	}
	if h != 0 {
		t.Fatalf("got %d, want 0", h)
	}
}

func TestOracleStore_NegativeHeightIgnoredByHigher(t *testing.T) {
	t.Parallel()
	s := NewOracleStore()

	s.Update("pocket", 100, "oracle-a")
	s.Update("pocket", -1, "oracle-b") // Negative — lower, should be ignored.

	h, _ := s.Get("pocket")
	if h != 100 {
		t.Fatalf("got %d, want 100", h)
	}
}
