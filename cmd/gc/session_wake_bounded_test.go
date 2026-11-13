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
	got := setMetadataBatchBounded(store, bead.ID, map[string]string{"test_k": "v"}, 50*time.Millisecond)
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
	if err := setMetadataBatchBounded(mem, bead.ID, map[string]string{"test_k": "v"}, time.Second); err != nil {
		t.Fatalf("setMetadataBatchBounded: %v", err)
	}
	got, err := mem.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["test_k"] != "v" {
		t.Errorf("metadata test_k = %q, want %q", got.Metadata["test_k"], "v")
	}
}

func TestBoundedStartWrite_ReturnsTheOpResult(t *testing.T) {
	got, err := boundedStartWrite(time.Second, func() (int, error) { return 7, nil })
	if err != nil || got != 7 {
		t.Fatalf("boundedStartWrite = (%d, %v), want (7, nil)", got, err)
	}
	wantErr := errors.New("op failed")
	if _, err := boundedStartWrite(time.Second, func() (int, error) { return 0, wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the op's error", err)
	}
}

func TestBoundedStartWrite_TimesOutAndDiscardsTheLateResult(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	start := time.Now()
	got, err := boundedStartWrite(50*time.Millisecond, func() (int, error) {
		<-release
		return 7, nil
	})
	if !errors.Is(err, errStartWriteTimedOut) || got != 0 {
		t.Fatalf("boundedStartWrite = (%d, %v), want (0, errStartWriteTimedOut)", got, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("bounded write took %s, expected fail-fast near the 50ms deadline", elapsed)
	}
}
