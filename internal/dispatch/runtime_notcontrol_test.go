package dispatch

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestProcessControlEmptyKindIsNotControlBead pins the gcw-zaey fix: a bead with
// no control kind is a plain work bead, not workflow infrastructure. ProcessControl
// must report ErrNotControlBead (so the caller SKIPS it, leaving it open and
// assignable) rather than the "unsupported control bead kind" hard error that
// closes it as a control-dispatch failure. A mis-scoped selection mass-quarantined
// 60 plain beads exactly this way.
func TestProcessControlEmptyKindIsNotControlBead(t *testing.T) {
	for _, kind := range []string{"", "   ", "\t"} {
		t.Run("kind="+strings.ReplaceAll(kind, "\t", "TAB"), func(t *testing.T) {
			store := beads.NewMemStore()
			md := map[string]string{}
			if kind != "" {
				md[beadmeta.KindMetadataKey] = kind
			}
			bead, err := store.Create(beads.Bead{Title: "plain work bead", Metadata: md})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			_, err = ProcessControl(store, bead, ProcessOptions{})
			if !errors.Is(err, ErrNotControlBead) {
				t.Fatalf("ProcessControl(empty kind) error = %v, want ErrNotControlBead", err)
			}
			if err != nil && strings.Contains(err.Error(), "unsupported control bead kind") {
				t.Fatalf("empty-kind bead hard-errored as unsupported kind: %v", err)
			}
		})
	}
}

// TestProcessControlUnknownNonEmptyKindStillHardErrors guards the other half of
// the distinction: a bead that DOES declare a kind but an unrecognized one is a
// genuine control-bead misconfiguration and must still hard-error (not be treated
// as a skippable non-control bead).
func TestProcessControlUnknownNonEmptyKindStillHardErrors(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:    "misconfigured control bead",
		Metadata: map[string]string{beadmeta.KindMetadataKey: "not-a-control-kind"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = ProcessControl(store, bead, ProcessOptions{})
	if errors.Is(err, ErrNotControlBead) {
		t.Fatalf("non-empty unknown kind returned ErrNotControlBead; want unsupported-kind hard error")
	}
	if err == nil || !strings.Contains(err.Error(), "unsupported control bead kind") {
		t.Fatalf("ProcessControl(unknown kind) error = %v, want unsupported-kind error", err)
	}
}
