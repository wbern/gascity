package bdshim

import "testing"

// TestClassifyDeleteNeverRoutes pins that `bd delete` always reaches real bd.
//
// The controller has no hard-delete: DELETE /v0/city/{city}/bead/{id} is a
// SOFT-delete implemented as store.Close ("Hard-delete is not exposed through
// the API" — internal/api/huma_handlers_beads.go:970-973). Routing bd's
// hard-delete onto it was wrong in both directions, measured live on the
// deployed shim:
//
//	bd delete <id>            raw bd: PREVIEW, mutates nothing (delete.go:465,
//	                          "Actually delete (without this flag, shows preview)")
//	                          shim:   CLOSED the bead, exit 0, no output
//	bd delete <id> --force    raw bd: deletes the issue
//	                          shim:   CLOSED the bead, exit 0, no output
//
// So a preview mutated state, and a delete left the bead in the store — in both
// cases silently, and reporting success. Nothing about this verb is expressible
// as a city-scoped controller call, so it must not be classified as routable at
// all.
func TestClassifyDeleteNeverRoutes(t *testing.T) {
	shapes := [][]string{
		{"gcw-1"},
		{"gcw-1", "--force"},
		{"gcw-1", "-f"},
		{"gcw-1", "--dry-run"},
		{"gcw-1", "--cascade", "--force"},
		{"gcw-1", "gcw-2", "--force"},
		{"--from-file", "ids.txt", "--force"},
	}
	for _, args := range shapes {
		if got := ClassifyVerb("delete", args, false); got != Passthrough {
			t.Errorf("ClassifyVerb(delete, %v) = %v, want Passthrough", args, got)
		}
	}
}

// TestClassifyCloseReopenRouteOnlyTheBareShape pins that close and reopen route
// ONLY `<verb> <id>` with no flags.
//
// The controller calls are CloseBead(id) and ReopenBead(id) — they carry no
// other input, so every flag bd accepts is silently discarded by routing.
// Measured live against a raw-bd control: `bd close <id> --reason "..."` stored
// close_reason=None through the shim and the full reason through real bd. The
// reason is the audit trail on a close, and it was being dropped at exit 0.
//
// Multi-id is the same silent-drop class fixed for update: routing served only
// the first id (`bd close A B` closed A, left B open, exit 0) while real bd
// closes both.
func TestClassifyCloseReopenRouteOnlyTheBareShape(t *testing.T) {
	cases := []struct {
		verb string
		args []string
		want Disposition
	}{
		{"close", []string{"gcw-1"}, Route},
		{"reopen", []string{"gcw-1"}, Route},

		// The measured reason-drop, and its documented aliases.
		{"close", []string{"gcw-1", "--reason", "why"}, Passthrough},
		{"close", []string{"gcw-1", "-r", "why"}, Passthrough},
		{"close", []string{"gcw-1", "--reason=why"}, Passthrough},
		{"close", []string{"gcw-1", "-m", "why"}, Passthrough},
		{"close", []string{"gcw-1", "--resolution", "why"}, Passthrough},
		{"close", []string{"gcw-1", "--comment", "why"}, Passthrough},
		{"close", []string{"gcw-1", "--reason-file", "r.md"}, Passthrough},
		{"reopen", []string{"gcw-1", "--reason", "why"}, Passthrough},

		// Flags that change what the command DOES, none of which the
		// controller call can express.
		{"close", []string{"gcw-1", "--force"}, Passthrough},
		{"close", []string{"gcw-1", "--continue"}, Passthrough},
		{"close", []string{"gcw-1", "--claim-next"}, Passthrough},
		{"close", []string{"gcw-1", "--suggest-next"}, Passthrough},
		{"close", []string{"gcw-1", "--session", "abc"}, Passthrough},

		// --json changes the OUTPUT contract: raw bd emits a JSON result and the
		// routed path prints nothing, so a parsing consumer reads empty.
		{"close", []string{"gcw-1", "--json"}, Passthrough},

		// Multi-id: real bd applies to every id, routing served only the first.
		{"close", []string{"gcw-1", "gcw-2"}, Passthrough},
		{"reopen", []string{"gcw-1", "gcw-2"}, Passthrough},

		// No id at all — bd falls back to its last-touched issue, which the
		// routed call cannot resolve.
		{"close", []string{}, Passthrough},
		{"reopen", []string{}, Passthrough},
	}
	for _, tc := range cases {
		if got := ClassifyVerb(tc.verb, tc.args, false); got != tc.want {
			t.Errorf("ClassifyVerb(%s, %v) = %v, want %v", tc.verb, tc.args, got, tc.want)
		}
	}
}
