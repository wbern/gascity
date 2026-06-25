package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// blockingMetadataStore wraps a MemStore but blocks every SetMetadataBatch
// until release is closed, simulating a backing-store stall (saturated Dolt
// connection pool) during a start-path write.
type blockingMetadataStore struct {
	*beads.MemStore
	release chan struct{}
}

func (s *blockingMetadataStore) SetMetadataBatch(id string, kvs map[string]string) error {
	<-s.release
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func TestSetMetadataBatchBounded_TimesOutWhenStoreBlocks(t *testing.T) {
	mem := beads.NewMemStore()
	bead, err := mem.Create(beads.Bead{Title: "s", Type: sessionBeadType})
	if err != nil {
		t.Fatal(err)
	}
	store := &blockingMetadataStore{MemStore: mem, release: make(chan struct{})}
	defer close(store.release) // let the leaked write goroutine finish after the test

	start := time.Now()
	got := setMetadataBatchBounded(store, bead.ID, map[string]string{"k": "v"}, 50*time.Millisecond)
	elapsed := time.Since(start)

	if !errors.Is(got, errStartWriteTimedOut) {
		t.Fatalf("err = %v, want errStartWriteTimedOut", got)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("bounded write took %s, expected fail-fast near the 50ms deadline", elapsed)
	}
}

func TestSetMetadataBatchBounded_PassesThroughOnSuccess(t *testing.T) {
	mem := beads.NewMemStore()
	bead, err := mem.Create(beads.Bead{Title: "s", Type: sessionBeadType})
	if err != nil {
		t.Fatal(err)
	}
	if err := setMetadataBatchBounded(mem, bead.ID, map[string]string{"k": "v"}, time.Second); err != nil {
		t.Fatalf("setMetadataBatchBounded: %v", err)
	}
	got, err := mem.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["k"] != "v" {
		t.Errorf("metadata k = %q, want %q", got.Metadata["k"], "v")
	}
}
