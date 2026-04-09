package storage

import (
	"sync"
	"testing"
)

func TestArchivalStore_SetAndGet(t *testing.T) {
	t.Parallel()
	s := NewArchivalStore()

	s.SetArchival("pocket", "seed-one")

	if !s.IsArchival("pocket", "seed-one") {
		t.Fatal("expected archival after SetArchival")
	}

	status := s.GetStatus("pocket", "seed-one")
	if status == nil {
		t.Fatal("expected non-nil status")
	}
	if !status.IsArchival {
		t.Fatal("expected IsArchival=true")
	}
	if status.LastCheck.IsZero() {
		t.Fatal("expected non-zero LastCheck")
	}
}

func TestArchivalStore_SetNotArchival(t *testing.T) {
	t.Parallel()
	s := NewArchivalStore()

	s.SetNotArchival("pocket", "seed-one")

	if s.IsArchival("pocket", "seed-one") {
		t.Fatal("expected not archival")
	}

	status := s.GetStatus("pocket", "seed-one")
	if status == nil {
		t.Fatal("expected non-nil status")
	}
	if status.IsArchival {
		t.Fatal("expected IsArchival=false")
	}
}

func TestArchivalStore_NeverChecked(t *testing.T) {
	t.Parallel()
	s := NewArchivalStore()

	if s.IsArchival("pocket", "seed-one") {
		t.Fatal("never-checked node should not be archival")
	}
	if s.GetStatus("pocket", "seed-one") != nil {
		t.Fatal("expected nil status for never-checked node")
	}
}

func TestArchivalStore_TransitionArchivalToNot(t *testing.T) {
	t.Parallel()
	s := NewArchivalStore()

	s.SetArchival("pocket", "seed-one")
	s.SetNotArchival("pocket", "seed-one")

	if s.IsArchival("pocket", "seed-one") {
		t.Fatal("expected not archival after transition")
	}
}

func TestArchivalStore_TransitionNotToArchival(t *testing.T) {
	t.Parallel()
	s := NewArchivalStore()

	s.SetNotArchival("pocket", "seed-one")
	s.SetArchival("pocket", "seed-one")

	if !s.IsArchival("pocket", "seed-one") {
		t.Fatal("expected archival after transition")
	}
}

func TestArchivalStore_IndependentNetworks(t *testing.T) {
	t.Parallel()
	s := NewArchivalStore()

	s.SetArchival("pocket", "node-1")
	s.SetNotArchival("ethereum", "node-1")

	if !s.IsArchival("pocket", "node-1") {
		t.Fatal("pocket node-1 should be archival")
	}
	if s.IsArchival("ethereum", "node-1") {
		t.Fatal("ethereum node-1 should not be archival")
	}
}

func TestArchivalStore_IndependentNodes(t *testing.T) {
	t.Parallel()
	s := NewArchivalStore()

	s.SetArchival("pocket", "seed-one")
	s.SetNotArchival("pocket", "seed-two")

	if !s.IsArchival("pocket", "seed-one") {
		t.Fatal("seed-one should be archival")
	}
	if s.IsArchival("pocket", "seed-two") {
		t.Fatal("seed-two should not be archival")
	}
}

func TestArchivalStore_GetStatus_ReturnsCopy(t *testing.T) {
	t.Parallel()
	s := NewArchivalStore()

	s.SetArchival("pocket", "seed-one")
	status := s.GetStatus("pocket", "seed-one")
	status.IsArchival = false

	if !s.IsArchival("pocket", "seed-one") {
		t.Fatal("original should be unaffected by copy mutation")
	}
}

func TestArchivalStore_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	s := NewArchivalStore()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			s.SetArchival("pocket", "seed-one")
		}()
		go func() {
			defer wg.Done()
			s.SetNotArchival("pocket", "seed-one")
		}()
		go func() {
			defer wg.Done()
			_ = s.IsArchival("pocket", "seed-one")
		}()
	}
	wg.Wait()

	// Should not panic. Final state indeterminate but valid.
	_ = s.GetStatus("pocket", "seed-one")
}
