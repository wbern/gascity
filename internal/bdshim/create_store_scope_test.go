package bdshim

import "testing"

// TestClassifyCreateNeverRoutes pins that `bd create` always reaches real bd.
//
// create is the only routed verb with no issue id, so it is the only one whose
// target store cannot be resolved from its arguments. Every other routed write
// names a bead the controller can locate; create names nothing, so routing it
// wrote to the controller's own city store no matter which store the caller
// meant. Reads do not follow: show and list always pass through to the
// BEADS_DIR the caller set. Write and read therefore disagreed.
//
// Measured live in a managed agent session, whose ambient env gc itself sets
// (BEADS_DIR=<rig>/.beads, GC_RIG=gas-city-wbern, GC_STORE_SCOPE unset):
//
//	bd create "probe" --json   -> {"id":"gc2-kxwt5"}   exit 0   (CITY store)
//	bd show gc2-kxwt5          -> no issue found       (reads the RIG store)
//	raw bd create "probe"      -> gcw-xqed             (RIG store, honors BEADS_DIR)
//
// So an agent could not read back the bead it had just created, and the id it
// was handed was unusable through the same CLI that minted it — at exit 0 in
// both directions.
//
// The cost of not routing is measured and negligible: over 595,180 shim calls,
// 4,185 creates already passed through and only 40 routed (0.95%), because
// CreateRoutable already rejects most real invocations.
func TestClassifyCreateNeverRoutes(t *testing.T) {
	shapes := [][]string{
		{"a title"},
		{"a title", "--json"},
		{"a title", "--type", "task"},
		{"a title", "--type", "task", "--priority", "1", "--json"},
		{"a title", "--assignee", "someone", "--label", "x"},
		{"a title", "--description", "d", "--parent", "gcw-1"},
		{"a title", "--metadata", `{"k":"v"}`},
	}
	for _, args := range shapes {
		if got := ClassifyVerb("create", args, false); got != Passthrough {
			t.Errorf("ClassifyVerb(create, %v) = %v, want Passthrough", args, got)
		}
	}
}

// TestCreateRoutableFlagsNoLongerGateRouting documents that CreateRoutable is
// retained only as a shape predicate: no caller may use it to reach the routed
// create path, because that path is gone. It also records the two entries that
// were wrong regardless — `bd create` has neither flag, so the shim was
// classifying as routable a shape real bd rejects outright:
//
//	bd create "t" --set-metadata a=1     Error: unknown flag: --set-metadata
//	bd create "t" --defer-until 2026-...  Error: unknown flag: --defer-until
//
// (bd's flags are --metadata and --defer.) --defer-until was the worse of the
// two: ParseCreateBead has no case for it, so routing accepted a flag bd would
// have refused and then silently dropped its effect while still creating the
// bead and exiting 0.
func TestCreateRoutableFlagsNoLongerGateRouting(t *testing.T) {
	if RoutedVerbs["create"] {
		t.Fatal("create is in RoutedVerbs; it must never route (no id => no resolvable store)")
	}
	// The shape predicate still answers, but its answer no longer changes the
	// disposition — every create passes through.
	if !CreateRoutable([]string{"t", "--type", "task"}) {
		t.Error("CreateRoutable should still recognize a mappable shape")
	}
	if got := ClassifyVerb("create", []string{"t", "--type", "task"}, false); got != Passthrough {
		t.Errorf("a CreateRoutable shape still classified as %v, want Passthrough", got)
	}
}
