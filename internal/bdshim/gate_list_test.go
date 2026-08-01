package bdshim

import "testing"

// TestClassifyGateRoutesOnlyTheListReadShape pins that only `bd gate list --json`
// routes, and every other gate invocation reaches real bd.
//
// gate is 13.9% of shim-facing traffic, but only 4,687 of 12,366 calls (38%) are
// the list read. The other 62% are `bd gate check`, which EVALUATES gates and
// CLOSES the resolved ones — a mutation with no controller equivalent. Routing
// it would be the delete class of defect all over again, so the shape allowlist
// is the whole safety property here.
//
// The read is routable now because the wire Bead finally carries bd's full gate
// projection: await_type landed first, then created_by and owner. Before those,
// routing this shape would have dropped fields silently.
func TestClassifyGateRoutesOnlyTheListReadShape(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		split bool
		want  Disposition
	}{
		{"the routable read", []string{"list", "--json"}, false, Route},
		{"with a limit", []string{"list", "--json", "--limit", "50"}, false, Route},
		{"with -n", []string{"list", "--json", "-n", "10"}, false, Route},
		{"inline limit", []string{"list", "--json=true"}, false, Passthrough},

		// bd gate check is a MUTATION: it closes resolved gates.
		{"check never routes", []string{"check"}, false, Passthrough},
		{"check with flags never routes", []string{"check", "--json"}, false, Passthrough},
		{"check escalate never routes", []string{"check", "--escalate"}, false, Passthrough},

		// Other subcommands are writes or unmodelled reads.
		{"create", []string{"create", "--blocks", "x"}, false, Passthrough},
		{"resolve", []string{"resolve", "x"}, false, Passthrough},
		{"add-waiter", []string{"add-waiter", "x"}, false, Passthrough},
		{"discover", []string{"discover"}, false, Passthrough},
		{"show", []string{"show", "x"}, false, Passthrough},

		// A human-output list has no JSON contract to reproduce.
		{"list without --json", []string{"list"}, false, Passthrough},
		// Unmodelled list filters must not be silently dropped.
		{"list with an unmodelled flag", []string{"list", "--json", "--status", "open"}, false, Passthrough},

		// Under the split phase an unrouted gate call still refuses loudly
		// rather than letting the work-only bd miss graph-resident gates.
		{"check under split refuses", []string{"check"}, true, Refuse},
		{"the read still routes under split", []string{"list", "--json"}, true, Route},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyVerb("gate", tc.args, tc.split); got != tc.want {
				t.Errorf("ClassifyVerb(gate, %v, split=%v) = %v, want %v", tc.args, tc.split, got, tc.want)
			}
		})
	}
}

// TestGateListLimit pins the limit parsed off the routable shape, since a
// dropped limit would silently widen the read.
func TestGateListLimit(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"list", "--json"}, 0},
		{[]string{"list", "--json", "--limit", "50"}, 50},
		{[]string{"list", "--json", "-n", "7"}, 7},
		{[]string{"list", "--limit=25", "--json"}, 25},
	}
	for _, tc := range cases {
		if got := GateListLimit(tc.args); got != tc.want {
			t.Errorf("GateListLimit(%v) = %d, want %d", tc.args, got, tc.want)
		}
	}
}
