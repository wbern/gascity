package main

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestControlDispatchSkipsNonControlBeadInsteadOfQuarantine is the end-to-end
// guard for gcw-zaey: when a plain work bead (no gc.kind) reaches the control
// dispatcher via a mis-scoped selection, it must be SKIPPED — left open and
// assignable — not hard-quarantined (closed, outcome=fail, never dispatched to
// its assignee). A mis-scoped selection mass-quarantined 60 plain beads this way.
func TestControlDispatchSkipsNonControlBeadInsteadOfQuarantine(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:    "plain P1 task assigned to a crewmate",
		Assignee: "gas-city-wbern/architect",
		// No gc.kind — this is a normal work bead, not control infrastructure.
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var stdout, stderr bytes.Buffer
	cityPath := t.TempDir()
	if err := runControlDispatcherWithStore(cityPath, "", store, bead, bead.ID, &stdout, &stderr); err != nil {
		t.Fatalf("runControlDispatcherWithStore returned error, want nil skip: %v", err)
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("get after dispatch: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("non-control bead status = %q, want it left open/assignable", got.Status)
	}
	if q := got.Metadata[beadmeta.ControlQuarantinedMetadataKey]; q != "" {
		t.Fatalf("non-control bead was control-quarantined (%s=%q); want untouched", beadmeta.ControlQuarantinedMetadataKey, q)
	}
	if d := got.Metadata[beadmeta.FinalDispositionMetadataKey]; d == beadmeta.DispositionControlQuarantine {
		t.Fatalf("non-control bead final_disposition = %q, want no quarantine disposition", d)
	}
}
