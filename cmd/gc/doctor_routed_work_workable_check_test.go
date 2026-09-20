package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
)

func TestRoutedWorkWorkableCheck_OkWhenAllWorkable(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStore()

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "builder"},
		},
	}

	_, err := store.Create(beads.Bead{
		Title:  "workable bead",
		Status: "open",
		Type:   "task",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "builder",
		},
	})
	if err != nil {
		t.Fatalf("Create bead: %v", err)
	}

	check := newRoutedWorkWorkableCheck(cfg, cityDir, func(_ string) (beads.Store, error) {
		return store, nil
	})

	res := check.Run(nil)
	if res.Status != doctor.StatusOK {
		t.Fatalf("check.Run() status = %v, want StatusOK; message = %s", res.Status, res.Message)
	}
}

func TestRoutedWorkWorkableCheck_DetectsAndFixesUnworkableBead(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStore()
	rec := events.NewFake()

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "builder"},
		},
		WorkKinds: map[string]config.WorkKind{
			"review": {
				RequireMetadata: []string{"molecule_id"},
				MatchMetadata:   []string{"review_context"},
			},
		},
	}

	// 1. Unresolvable route target
	b1, err := store.Create(beads.Bead{
		Title:  "unresolvable target",
		Status: "open",
		Type:   "task",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "nonexistent",
		},
	})
	if err != nil {
		t.Fatalf("Create b1: %v", err)
	}

	// 2. Missing required metadata
	b2, err := store.Create(beads.Bead{
		Title:  "missing metadata",
		Status: "open",
		Type:   "task",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "builder",
			"review_context":             "code-review",
		},
	})
	if err != nil {
		t.Fatalf("Create b2: %v", err)
	}

	// 3. Human-routed bead (must NOT be flagged)
	bHuman, err := store.Create(beads.Bead{
		Title:  "human routed",
		Status: "open",
		Type:   "task",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "human",
		},
	})
	if err != nil {
		t.Fatalf("Create bHuman: %v", err)
	}

	// 4. Gate routed to pool (unreachable by readiness)
	bGate, err := store.Create(beads.Bead{
		Title:  "gate routed to pool",
		Status: "open",
		Type:   "gate",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "builder",
		},
	})
	if err != nil {
		t.Fatalf("Create bGate: %v", err)
	}

	check := newRoutedWorkWorkableCheck(cfg, cityDir, func(_ string) (beads.Store, error) {
		return store, nil
	})
	check.eventsRec = rec

	// Run detection
	res := check.Run(nil)
	if res.Status != doctor.StatusWarning {
		t.Fatalf("check.Run() status = %v, want StatusWarning", res.Status)
	}
	if len(res.Details) != 3 {
		t.Fatalf("check.Run() found %d details, want 3 (b1, b2, bGate): %v", len(res.Details), res.Details)
	}

	// Run fix
	err = check.Fix(nil)
	if err != nil {
		t.Fatalf("check.Fix(): %v", err)
	}

	// Verify b1 was parked: routed_to cleared, reason stamped, deferred, NOT closed
	gotB1, err := store.Get(b1.ID)
	if err != nil {
		t.Fatalf("store.Get(b1): %v", err)
	}
	if gotB1.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		t.Errorf("b1 gc.routed_to = %q, want empty", gotB1.Metadata[beadmeta.RoutedToMetadataKey])
	}
	if gotB1.Metadata[beadmeta.UnworkableReasonMetadataKey] == "" {
		t.Errorf("b1 gc.unworkable_reason is empty")
	}
	if gotB1.Status != "open" {
		t.Errorf("b1 status = %q, want 'open' (ParkBead must never close)", gotB1.Status)
	}
	if gotB1.DeferUntil == nil || gotB1.DeferUntil.Before(time.Now()) {
		t.Errorf("b1 DeferUntil = %v, want future deferral", gotB1.DeferUntil)
	}

	// Verify b2 was also parked
	gotB2, err := store.Get(b2.ID)
	if err != nil {
		t.Fatalf("store.Get(b2): %v", err)
	}
	if gotB2.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		t.Errorf("b2 gc.routed_to = %q, want empty", gotB2.Metadata[beadmeta.RoutedToMetadataKey])
	}

	// Verify bGate was also parked
	gotBGate, err := store.Get(bGate.ID)
	if err != nil {
		t.Fatalf("store.Get(bGate): %v", err)
	}
	if gotBGate.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		t.Errorf("bGate gc.routed_to = %q, want empty", gotBGate.Metadata[beadmeta.RoutedToMetadataKey])
	}

	// Verify bHuman was untouched
	gotHuman, err := store.Get(bHuman.ID)
	if err != nil {
		t.Fatalf("store.Get(bHuman): %v", err)
	}
	if gotHuman.Metadata[beadmeta.RoutedToMetadataKey] != "human" {
		t.Errorf("bHuman was altered, routed_to = %q", gotHuman.Metadata[beadmeta.RoutedToMetadataKey])
	}

	// Verify events were emitted: 3 unworkable + 3 parked = 6 events
	evs, err := rec.List(events.Filter{})
	if err != nil {
		t.Fatalf("rec.List: %v", err)
	}
	if len(evs) != 6 {
		t.Fatalf("emitted %d events, want 6", len(evs))
	}

	// Rerun check - should now be clean!
	resClean := check.Run(nil)
	if resClean.Status != doctor.StatusOK {
		t.Fatalf("after fix: check.Run() status = %v, want StatusOK; message = %s", resClean.Status, resClean.Message)
	}
}
