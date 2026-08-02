package bdshim

import "testing"

// A routed `bd ready` is rendered by internal/bddispatch, which implements only
// the JSON projection. Real bd renders a compact human table when --json is
// absent. So routing a ready invocation that did NOT ask for JSON silently
// replaces a short list with a full-store JSON dump.
//
// Measured on the deployed shim, 2026-08-02 (gcw-tcuk):
//
//	bd ready   via shim -> exit 0, 2,736,285 bytes, `[{"id":"gc2-gs12i",...`
//	bd ready   raw bd   -> exit 0,     13,656 bytes, `○ gcw-hegp ● P0 ...`
//
// and ~/gc2/.gc/bdshim.log showed 9,721 of 33,060 ready calls taking that path —
// `disposition=route shape=<empty>` was the single most common ready shape in
// 605,600 records, i.e. the modal agent invocation rather than an edge case.
//
// This is the mirror of the rule already applied to close/reopen, where --json
// is EXCLUDED from the routable set because the routed path prints nothing while
// raw bd prints a JSON result. Same principle, opposite direction: route only
// the output form we can actually reproduce.
func TestReadyWithoutJSONIsNotRouted(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"bare", nil},
		{"empty slice", []string{}},
		{"assignee only", []string{"--assignee=worker"}},
		{"limit only", []string{"--limit", "5"}},
		{"unassigned only", []string{"--unassigned"}},
		{"short limit only", []string{"-n", "3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ReadyRoutable(tc.args) {
				t.Errorf("ReadyRoutable(%q) = true, want false: the routed renderer emits JSON, "+
					"but real bd emits a human table when --json is absent", tc.args)
			}
			if got := ClassifyVerb("ready", tc.args, false); got != Passthrough {
				t.Errorf("ClassifyVerb(ready, %q) = %v, want Passthrough", tc.args, got)
			}
		})
	}
}

// The JSON form must keep routing — that is the hot automation path (23,317 of
// 33,060 observed ready calls) and the whole point of the fast route.
func TestReadyWithJSONStillRoutes(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"json only", []string{"--json"}},
		{"json and limit", []string{"--json", "--limit", "1"}},
		{"assignee json limit", []string{"--assignee=w", "--json", "--limit", "1"}},
		{"discovery post-filter shape", []string{"--metadata-field", "gc.routed_to=x", "--unassigned", "--json"}},
		{"json equals form", []string{"--json=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !ReadyRoutable(tc.args) {
				t.Errorf("ReadyRoutable(%q) = false, want true", tc.args)
			}
			if got := ClassifyVerb("ready", tc.args, false); got != Route {
				t.Errorf("ClassifyVerb(ready, %q) = %v, want Route", tc.args, got)
			}
		})
	}
}

// An unroutable flag must still win over the presence of --json, so the existing
// federation boundary is unchanged by the output-form rule.
func TestReadyUnroutableFlagStillPassesThroughEvenWithJSON(t *testing.T) {
	args := []string{"--label", "pool:worker", "--json"}
	if ReadyRoutable(args) {
		t.Fatalf("ReadyRoutable(%q) = true, want false (--label is not federated)", args)
	}
	if got := ClassifyVerb("ready", args, false); got != Passthrough {
		t.Errorf("ClassifyVerb(ready, %q) = %v, want Passthrough", args, got)
	}
}
