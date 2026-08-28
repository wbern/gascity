package beads

import (
	"errors"
	"testing"
)

func TestResolveHumanGateIfCurrentRejectsChangedConversationWithoutClosing(t *testing.T) {
	store := NewMemStore()
	target, err := store.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := store.Create(Bead{
		Title:       "Approve release",
		Description: "Choose one option",
		Type:        "gate",
		AwaitType:   "human",
		Metadata:    StringMap{"options": "approve,reject", "recommendation": "approve"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(target.ID, gate.ID, "blocks"); err != nil {
		t.Fatal(err)
	}

	expected := HumanGateContractFrom(gate, target.ID)
	if err := store.Update(gate.ID, UpdateOpts{Metadata: map[string]string{"recommendation": "reject"}}); err != nil {
		t.Fatal(err)
	}

	err = ResolveHumanGateIfCurrent(store, expected)
	var stale *StaleHumanGateError
	if !errors.As(err, &stale) {
		t.Fatalf("ResolveHumanGateIfCurrent error = %v, want *StaleHumanGateError", err)
	}
	if stale.Reason != HumanGateStaleContractChanged {
		t.Fatalf("stale reason = %q, want %q", stale.Reason, HumanGateStaleContractChanged)
	}
	current, err := store.Get(gate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "open" {
		t.Fatalf("gate status = %q, want open", current.Status)
	}
}

func TestResolveHumanGateIfCurrentRequiresAuthoritativeBlockingTarget(t *testing.T) {
	store := NewMemStore()
	target, err := store.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := store.Create(Bead{Title: "Approve", Type: "gate", AwaitType: "human"})
	if err != nil {
		t.Fatal(err)
	}

	err = ResolveHumanGateIfCurrent(store, HumanGateContractFrom(gate, target.ID))
	var stale *StaleHumanGateError
	if !errors.As(err, &stale) || stale.Reason != HumanGateStaleTargetChanged {
		t.Fatalf("ResolveHumanGateIfCurrent error = %v, want stale target", err)
	}
	current, err := store.Get(gate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "open" {
		t.Fatalf("gate status = %q, want open", current.Status)
	}
}

func TestResolveHumanGateIfCurrentClosesMatchingHumanGate(t *testing.T) {
	store := NewMemStore()
	target, err := store.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := store.Create(Bead{Title: "Approve", Type: "gate", AwaitType: "human"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(target.ID, gate.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	gate, err = store.Get(gate.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := ResolveHumanGateIfCurrent(store, HumanGateContractFrom(gate, target.ID)); err != nil {
		t.Fatalf("ResolveHumanGateIfCurrent: %v", err)
	}
	current, err := store.Get(gate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "closed" {
		t.Fatalf("gate status = %q, want closed", current.Status)
	}
}

func TestResolveHumanGateIfCurrentUsesNativeDoltTransaction(t *testing.T) {
	store := newNativeDoltStoreWithStorage(newNativeDoltMemStorage(), "test")
	target, err := store.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := store.Create(Bead{Title: "Approve", Type: "gate", AwaitType: "human"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(target.ID, gate.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	gate, err = store.Get(gate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gate.Type != "gate" || gate.AwaitType != "human" {
		t.Fatalf("native gate = %#v, want type gate and await human", gate)
	}

	if err := ResolveHumanGateIfCurrent(store, HumanGateContractFrom(gate, target.ID)); err != nil {
		t.Fatalf("ResolveHumanGateIfCurrent: %v", err)
	}
	current, err := store.Get(gate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "closed" {
		t.Fatalf("gate status = %q, want closed", current.Status)
	}
}

func TestResolveHumanGateIfCurrentForwardsThroughCache(t *testing.T) {
	backing := NewMemStore()
	store := NewCachingStoreForTest(backing, nil)
	target, err := backing.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := backing.Create(Bead{Title: "Approve", Type: "gate", AwaitType: "human"})
	if err != nil {
		t.Fatal(err)
	}
	if err := backing.DepAdd(target.ID, gate.ID, "blocks"); err != nil {
		t.Fatal(err)
	}

	if err := ResolveHumanGateIfCurrent(store, HumanGateContractFrom(gate, target.ID)); err != nil {
		t.Fatalf("ResolveHumanGateIfCurrent: %v", err)
	}
	current, err := backing.Get(gate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "closed" {
		t.Fatalf("backing gate status = %q, want closed", current.Status)
	}
}
