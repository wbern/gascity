package bdshim

import (
	"reflect"
	"testing"
)

// TestUpdatePositionals pins that positional extraction skips tokens consumed as
// a flag's value. Reading the first non-flag token as the id (the previous
// FirstBdPositional rule) picked `a=1` out of `--set-metadata a=1 <id>`, so a
// flags-first invocation — which cobra accepts and raw bd honors — targeted a
// bead named after the metadata pair.
func TestUpdatePositionals(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"id only", []string{"gcw-1"}, []string{"gcw-1"}},
		{"id then space-separated flag", []string{"gcw-1", "--status", "closed"}, []string{"gcw-1"}},
		{"id then inline flag", []string{"gcw-1", "--status=closed"}, []string{"gcw-1"}},
		{"flag value precedes id", []string{"--set-metadata", "a=1", "gcw-1"}, []string{"gcw-1"}},
		{"bool flag does not consume", []string{"--claim", "gcw-1"}, []string{"gcw-1"}},
		{"multiple ids", []string{"gcw-1", "gcw-2", "--status=closed"}, []string{"gcw-1", "gcw-2"}},
		{
			"repeated set-metadata keeps one positional",
			[]string{"gcw-1", "--set-metadata", "a=1", "--set-metadata", "b=2"},
			[]string{"gcw-1"},
		},
		{
			"bare pairs after the first are positionals",
			[]string{"gcw-1", "--set-metadata", "a=1", "b=2", "c=3"},
			[]string{"gcw-1", "b=2", "c=3"},
		},
		{"no positional at all", []string{"--status=closed"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UpdatePositionals(tc.args); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("UpdatePositionals(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestUpdateMistypedMetadataPair pins the guard on the shape that motivated it:
// `--set-metadata` is a repeatable stringArray taking ONE pair per flag, so every
// bare k=v token after the first lands in bd's positional issue-id slot. Raw bd
// resolves it, fails, prints to stderr, still prints the success line and EXITS
// 0 — so a caller cannot tell a full write from a 1-of-N write. No bead id
// contains '=' (verified: 3838 live beads across two stores, zero matches), so a
// '='-bearing positional was ALWAYS an unresolvable id. Refusing it cannot break
// an invocation that previously worked; it only converts a silent partial write
// into a loud refusal before anything is written.
func TestUpdateMistypedMetadataPair(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"the reported defect", []string{"gcw-1", "--set-metadata", "a=1", "b=2", "c=3"}, true},
		{"single trailing pair", []string{"gcw-1", "--set-metadata", "a=1", "b=2"}, true},
		{"inline flag form drops pairs too", []string{"gcw-1", "--set-metadata=a=1", "b=2"}, true},
		{"correct repeated-flag form is clean", []string{"gcw-1", "--set-metadata", "a=1", "--set-metadata", "b=2"}, false},
		{"plain id is clean", []string{"gcw-1", "--status=closed"}, false},
		{"legitimate multi-id is clean", []string{"gcw-1", "gcw-2", "--set-metadata", "a=1"}, false},
		{"flag value is never a positional", []string{"--set-metadata", "a=1", "gcw-1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UpdateMistypedMetadataPair(tc.args); got != tc.want {
				t.Errorf("UpdateMistypedMetadataPair(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestClassifyUpdatePositionalShapes pins the dispositions that close the
// silent-drop class. The routed update path can carry exactly ONE id, so any
// other count must reach real bd, which applies the update to every id — routing
// a two-id update served only the first and reported success (measured live:
// `bd update A B --set-metadata multi=yes` left B untouched, exit 0).
func TestClassifyUpdatePositionalShapes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want Disposition
	}{
		{"single id routes", []string{"gcw-1", "--set-metadata", "a=1"}, Route},
		{"mistyped pairs refuse before any write", []string{"gcw-1", "--set-metadata", "a=1", "b=2"}, Refuse},
		{"multi-id passes through to real bd", []string{"gcw-1", "gcw-2", "--status=closed"}, Passthrough},
		// The id-less form keeps routing so its existing loud downstream refusal
		// stands: this guard closes the silent-drop class and deliberately does
		// not hand id-less writes bd's last-touched fallback.
		{"id-less update keeps its existing refusal", []string{"--status=closed"}, Route},
		{"multi-id claim passes through", []string{"gcw-1", "gcw-2", "--claim"}, Passthrough},
		{"single-id claim still routes", []string{"gcw-1", "--claim"}, Route},
		{"flags-first single id routes", []string{"--set-metadata", "a=1", "gcw-1"}, Route},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyVerb("update", tc.args, false); got != tc.want {
				t.Errorf("ClassifyVerb(update, %v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
