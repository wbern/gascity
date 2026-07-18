package bdshim

import "testing"

// TestClassifyVerb pins the three-way disposition policy: routed verbs always
// route; provably-graph-free verbs always passthrough; graph-touching unrouted
// verbs passthrough in the identity phase (byte-identical, safe) but are refused
// in the split phase rather than silently bypassing the graph store.
func TestClassifyVerb(t *testing.T) {
	cases := []struct {
		verb  string
		args  []string
		split bool
		want  Disposition
	}{
		{"close", []string{"x"}, false, Route},
		{"close", []string{"x"}, true, Route},
		{"show", []string{"x", "--json"}, true, Route},
		{"version", nil, false, Passthrough},
		{"version", nil, true, Passthrough},
		{"mol", []string{"current", "m"}, false, Route},  // current|progress + id routes (graph-aware) in both phases
		{"mol", []string{"current", "m"}, true, Route},   // split phase: routes to GET /beads/graph/{root}
		{"mol", []string{"pour", "proto"}, true, Refuse}, // non-read mol subcommand: refuse under split
		{"mol", []string{"current"}, true, Refuse},       // id-omitted (bd infers it): not routable, refuse under split
		{"gate", []string{"check"}, true, Refuse},
		{"query", []string{"ephemeral=true"}, true, Refuse}, // no --json: not routable, refuse under split
		// ready: the simple assigned form routes (graph-aware), but predicate
		// flags the Router cannot yet replicate (pool-demand; C3/ga-2gap48.11)
		// passthrough to the work-only bd — byte-identical in the identity phase.
		{"ready", []string{"--assignee=w", "--json", "--limit", "1"}, true, Route},
		// Discovery predicates now route (C3): the shim federates store.Ready() and
		// post-filters, so a graph control bead in SQLite is discoverable.
		{"ready", []string{"--metadata-field", "gc.routed_to=x", "--unassigned", "--json"}, true, Route},
		{"ready", []string{"--exclude-type=epic", "--json"}, false, Route},
		// A ready flag the shim does not model still passes through (byte-identical).
		{"ready", []string{"--label", "pool:worker", "--json"}, true, Passthrough},
		// update: the cleanly-mappable flag set routes (the canonical graph-worker
		// close), but flags with no UpdateOpts mapping (--notes/--claim/...)
		// passthrough — byte-identical in the identity phase.
		{"update", []string{"x", "--set-metadata", "gc.outcome=pass", "--status", "closed"}, true, Route},
		{"update", []string{"x", "--notes", "done", "--status=closed"}, true, Passthrough},
		// claim: the pure-claim shape (optionally --json) now routes to the warm
		// controller (POST /bead/{id}/claim); runBdShim gates on BEADS_ACTOR and
		// falls back to real bd when the actor is unset or the backend can't claim.
		{"update", []string{"x", "--claim"}, true, Route},
		{"update", []string{"x", "--claim", "--json"}, true, Route},
		// claim combined with another mutation has no atomic claim-route translation:
		// passthrough to real bd (byte-identical in the identity phase).
		{"update", []string{"x", "--claim", "--status", "closed"}, true, Passthrough},
		{"reopen", []string{"x"}, true, Route},
		{"delete", []string{"x", "--force"}, true, Route},
	}
	for _, tc := range cases {
		if got := ClassifyVerb(tc.verb, tc.args, tc.split); got != tc.want {
			t.Errorf("ClassifyVerb(%q, %v, split=%v) = %v, want %v", tc.verb, tc.args, tc.split, got, tc.want)
		}
	}
}

// TestClassifyVerbListRouting pins the `bd list` routing gate: the cache-servable
// shapes route, everything else passes through byte-identically.
func TestClassifyVerbListRouting(t *testing.T) {
	cases := []struct {
		args []string
		want Disposition
	}{
		// The GUPP-hook AssignedInProgressQuery — the dominant live shape.
		{[]string{"--status", "in_progress", "--assignee=gc2-x", "--json", "--limit", "50"}, Route},
		{[]string{"--status", "in_progress", "--json"}, Route},
		{[]string{"-s", "open", "-a", "y", "-n", "10", "--json"}, Route},
		{[]string{"--all", "--json"}, Route},
		// --json is REQUIRED: raw `bd list` defaults to a human tree, so a
		// non-json list must passthrough to preserve the output shape.
		{[]string{"--status", "in_progress"}, Passthrough},
		// Flags api.ListBeadsOpts cannot express passthrough (the refinery
		// --metadata-field/--exclude-type shape, --offset, --sort, --no-assignee).
		{[]string{"--metadata-field", "pr_number=5", "--exclude-type=epic", "--json"}, Passthrough},
		{[]string{"--json", "--offset", "10"}, Passthrough},
		{[]string{"--json", "--no-assignee"}, Passthrough},
	}
	for _, tc := range cases {
		if got := ClassifyVerb("list", tc.args, false); got != tc.want {
			t.Errorf("ClassifyVerb(list, %v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestClassifyVerbQueryRoutes: a mappable ephemeral query routes in both phases;
// an unmappable one refuses under split (would miss SQLite wisps) and passes
// through in the identity phase.
func TestClassifyVerbQueryRoutes(t *testing.T) {
	routable := []string{"--json", "ephemeral=true AND status=open"}
	unmappable := []string{"--json", "type=bug"}
	if QueryRoutingEnabled {
		// v2: a mappable ephemeral query routes in both phases.
		if got := ClassifyVerb("query", routable, true); got != Route {
			t.Fatalf("routable query (split) = %v, want Route", got)
		}
		if got := ClassifyVerb("query", routable, false); got != Route {
			t.Fatalf("routable query (identity) = %v, want Route", got)
		}
	} else {
		// v1: query never routes (GET /beads/ephemeral is not ported); it passes
		// through in the identity phase and refuses under split, so a
		// SQLite-resident wisp is never silently missed.
		if got := ClassifyVerb("query", routable, false); got != Passthrough {
			t.Fatalf("routable query (identity, v1) = %v, want Passthrough", got)
		}
		if got := ClassifyVerb("query", routable, true); got != Refuse {
			t.Fatalf("routable query (split, v1) = %v, want Refuse", got)
		}
	}
	// The unmappable-query contract is identical regardless of routing.
	if got := ClassifyVerb("query", unmappable, true); got != Refuse {
		t.Fatalf("unmappable query (split) = %v, want Refuse", got)
	}
	if got := ClassifyVerb("query", unmappable, false); got != Passthrough {
		t.Fatalf("unmappable query (identity) = %v, want Passthrough", got)
	}
}

// TestMolRoutableArgs covers the routable read shapes and the forms that must
// not route (other subcommands, omitted id, view flags).
func TestMolRoutableArgs(t *testing.T) {
	cases := []struct {
		args []string
		ok   bool
	}{
		{[]string{"current", "gcg-1"}, true},
		{[]string{"progress", "gcg-1"}, true},
		{[]string{"current", "gcg-1", "--json"}, true},
		{[]string{"current"}, false},                            // id omitted (bd infers it)
		{[]string{"pour", "proto"}, false},                      // not a read subcommand
		{[]string{"current", "--for", "agent", "gcg-1"}, false}, // view flag: not routable
		{nil, false},
	}
	for _, tc := range cases {
		if got := MolRoutableArgs(tc.args); got != tc.ok {
			t.Errorf("MolRoutableArgs(%v) = %v, want %v", tc.args, got, tc.ok)
		}
	}
}

// TestSplitGlobalFlags proves the shim finds the bd subcommand past leading
// global flags — the controller discovers work via `bd --readonly --sandbox
// ready ...`, where the verb is not args[0].
func TestSplitGlobalFlags(t *testing.T) {
	cases := []struct {
		args []string
		verb string
		rest []string
	}{
		{[]string{"--readonly", "--sandbox", "ready", "--json"}, "ready", []string{"--json"}},
		{[]string{"ready", "--assignee=x"}, "ready", []string{"--assignee=x"}},
		{[]string{"close", "id"}, "close", []string{"id"}},
		{[]string{"--readonly"}, "", nil},
		{nil, "", nil},
	}
	for _, tc := range cases {
		verb, rest := SplitGlobalFlags(tc.args)
		if verb != tc.verb {
			t.Errorf("SplitGlobalFlags(%v) verb = %q, want %q", tc.args, verb, tc.verb)
		}
		if len(rest) != len(tc.rest) {
			t.Errorf("SplitGlobalFlags(%v) rest = %v, want %v", tc.args, rest, tc.rest)
			continue
		}
		for i := range rest {
			if rest[i] != tc.rest[i] {
				t.Errorf("SplitGlobalFlags(%v) rest = %v, want %v", tc.args, rest, tc.rest)
				break
			}
		}
	}
}

// TestQueryRoutable pins the pure routability half of parseBdQueryEphemeral: the
// two in-repo ephemeral shapes route; predicate/flag forms outside the closed
// allowlist do not. It mirrors cmd/gc's TestParseBdQueryEphemeral on the boolean.
func TestQueryRoutable(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"listEphemeral shape", []string{"query", "--json", "ephemeral=true AND status=open AND label=wisp_type:ping", "--limit", "0"}, true},
		{"work_query literal", []string{"query", "--json", "ephemeral=true AND status=in_progress", "--limit=0"}, true},
		{"with --all", []string{"query", "--json", "ephemeral=true", "--all"}, true},
		{"missing --json", []string{"query", "ephemeral=true"}, false},
		{"non-ephemeral predicate", []string{"query", "--json", "type=bug"}, false},
		{"non-bare value", []string{"query", "--json", "ephemeral=true AND status=open OR x"}, false},
		{"unknown flag", []string{"query", "--json", "ephemeral=true", "--weird"}, false},
		{"bad limit int", []string{"query", "--json", "ephemeral=true", "--limit", "notanint"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := QueryRoutable(tc.args); got != tc.want {
				t.Fatalf("QueryRoutable(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestDispositionString pins the telemetry token each disposition renders as.
func TestDispositionString(t *testing.T) {
	cases := map[Disposition]string{
		Passthrough: "passthrough",
		Route:       "route",
		Refuse:      "refuse",
	}
	for d, want := range cases {
		if got := d.String(); got != want {
			t.Errorf("Disposition(%d).String() = %q, want %q", d, got, want)
		}
	}
}
