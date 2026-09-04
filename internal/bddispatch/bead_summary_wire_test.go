package bddispatch

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestSummaryRoutingMetadataKeysPinned pins the on-store spelling of every key
// the summary projection selects, in order.
//
// These names are read out of bead.Metadata as it exists in the store, so they
// are a wire contract, not an implementation detail: a key that changes
// spelling stops matching and the field silently vanishes from the projection
// rather than failing loudly. The list is written in terms of beadmeta
// constants so the vocabulary stays compiler-checked, which means a rename of
// a constant's VALUE would propagate here unnoticed. This test is the
// value-side backstop the constants cannot provide themselves.
func TestSummaryRoutingMetadataKeysPinned(t *testing.T) {
	want := []string{
		"gc.routed_to",
		"gc.root_bead_id",
		"gc.session_id",
		"gc.session_name",
		"gc.step_id",
		"target",
		"branch",
		"gc.base_sha",
		"gc.task_worktree",
		"work_dir",
		"gc.workspace_owner",
	}
	if !slices.Equal(summaryRoutingMetadataKeys, want) {
		t.Fatalf("summaryRoutingMetadataKeys drifted:\n got: %q\nwant: %q", summaryRoutingMetadataKeys, want)
	}
}

// TestBeadSummaryKindPinned pins the envelope discriminator. Readers reject an
// envelope whose kind they do not recognize, so a changed value turns every
// bounded discovery response into an unparseable one.
func TestBeadSummaryKindPinned(t *testing.T) {
	if BeadSummaryKind != "gc.bead_summary" {
		t.Fatalf("BeadSummaryKind = %q, want %q", BeadSummaryKind, "gc.bead_summary")
	}
}

// TestBeadSummaryProjectsEveryPinnedRoutingKey proves the pinned names are the
// names actually honored end to end: a bead carrying all of them must emit all
// of them. A pin that no projection consults would be decoration.
func TestBeadSummaryProjectsEveryPinnedRoutingKey(t *testing.T) {
	metadata := beads.StringMap{}
	for _, key := range summaryRoutingMetadataKeys {
		metadata[key] = "v-" + key
	}
	// An undeclared neighbor must NOT be projected: the selection is an
	// allowlist, not a passthrough that happens to include the pinned keys.
	metadata["gc.description"] = "secret"

	envelope := NewBeadSummaryEnvelope("list", []beads.Bead{{
		ID:       "gcw-1",
		Status:   "open",
		Metadata: metadata,
	}}, DefaultBeadSummaryBudget)

	if envelope.Kind != BeadSummaryKind {
		t.Fatalf("envelope.Kind = %q, want %q", envelope.Kind, BeadSummaryKind)
	}
	if len(envelope.Beads) != 1 {
		t.Fatalf("envelope.Beads = %d, want 1", len(envelope.Beads))
	}
	got := envelope.Beads[0].RoutingMetadata
	for _, key := range summaryRoutingMetadataKeys {
		if got[key] != "v-"+key {
			t.Errorf("RoutingMetadata[%q] = %q, want %q", key, got[key], "v-"+key)
		}
	}
	if _, leaked := got["gc.description"]; leaked {
		t.Error("RoutingMetadata leaked an unselected metadata key")
	}
	if len(got) != len(summaryRoutingMetadataKeys) {
		t.Errorf("RoutingMetadata has %d keys, want exactly %d", len(got), len(summaryRoutingMetadataKeys))
	}

	// The projection is what reaches a reader, so assert on the serialized form
	// rather than only the in-memory struct.
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var round BeadSummaryEnvelope
	if err := json.Unmarshal(payload, &round); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if round.Kind != BeadSummaryKind {
		t.Fatalf("round-tripped Kind = %q, want %q", round.Kind, BeadSummaryKind)
	}
	for _, key := range summaryRoutingMetadataKeys {
		if round.Beads[0].RoutingMetadata[key] != "v-"+key {
			t.Errorf("round-tripped RoutingMetadata[%q] = %q, want %q", key, round.Beads[0].RoutingMetadata[key], "v-"+key)
		}
	}
}

// TestShowSummarySelectsPinnedRoutingKeys covers the show projection, which
// consults the same list through a different function than the list/ready path.
func TestShowSummarySelectsPinnedRoutingKeys(t *testing.T) {
	metadata := beads.StringMap{}
	for _, key := range summaryRoutingMetadataKeys {
		metadata[key] = "v-" + key
	}
	metadata["gc.description"] = "secret"

	got := NewBeadShowSummaries([]beads.Bead{{ID: "gcw-1", Status: "open", Metadata: metadata}})
	if len(got) != 1 {
		t.Fatalf("NewBeadShowSummaries() = %d summaries, want 1", len(got))
	}
	for _, key := range summaryRoutingMetadataKeys {
		if got[0].Metadata[key] != "v-"+key {
			t.Errorf("Metadata[%q] = %q, want %q", key, got[0].Metadata[key], "v-"+key)
		}
	}
	if _, leaked := got[0].Metadata["gc.description"]; leaked {
		t.Error("show Metadata leaked an unselected metadata key")
	}
}
