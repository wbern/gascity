package bdshim

import (
	"reflect"
	"testing"
)

// TestSplitGlobalFlagsSkipsGlobalFlagValues pins that a global flag's VALUE is
// never mistaken for the subcommand.
//
// bd's global value-flags sit before the verb, and taking the first non-dash
// token as the verb read their value instead:
//
//	bd --actor bob update <id> --set-metadata a=1 b=2   -> verb "bob"
//
// which classified as Passthrough, so raw bd performed the exact silent 1-of-N
// write the mistyped-pair guard exists to prevent. Every guard keyed off the
// verb — the metadata guard, the delete/create routing bans, the split-phase
// refusals — was bypassed by the same token.
func TestSplitGlobalFlagsSkipsGlobalFlagValues(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantVerb string
		wantRest []string
	}{
		{"plain", []string{"update", "gcw-1"}, "update", []string{"gcw-1"}},
		{"actor", []string{"--actor", "bob", "update", "gcw-1"}, "update", []string{"gcw-1"}},
		{"-C dir", []string{"-C", "/some/dir", "update", "gcw-1"}, "update", []string{"gcw-1"}},
		{"--db", []string{"--db", "/x/y.db", "update", "gcw-1"}, "update", []string{"gcw-1"}},
		{"--directory", []string{"--directory", "/d", "delete", "gcw-1"}, "delete", []string{"gcw-1"}},
		{"--dolt-auto-commit", []string{"--dolt-auto-commit", "off", "create", "t"}, "create", []string{"t"}},
		{"inline form consumes nothing", []string{"--actor=bob", "update", "gcw-1"}, "update", []string{"gcw-1"}},
		{"bool global still works", []string{"--json", "update", "gcw-1"}, "update", []string{"gcw-1"}},
		{"several globals", []string{"--actor", "bob", "--json", "-C", "/d", "update", "gcw-1"}, "update", []string{"gcw-1"}},
		{"no verb", []string{"--actor", "bob"}, "", nil},
		{"empty", nil, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verb, rest := SplitGlobalFlags(tc.args)
			if verb != tc.wantVerb {
				t.Errorf("verb = %q, want %q", verb, tc.wantVerb)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tc.wantRest)
			}
		})
	}
}

// TestGuardSurvivesGlobalFlagPrefix pins the end-to-end consequence: the
// mistyped-pair guard must fire even when a global value-flag precedes the verb.
func TestGuardSurvivesGlobalFlagPrefix(t *testing.T) {
	verb, rest := SplitGlobalFlags([]string{"--actor", "bob", "update", "gcw-1", "--set-metadata", "a=1", "b=2"})
	if verb != "update" {
		t.Fatalf("verb = %q, want update", verb)
	}
	if !UpdateMistypedMetadataPair(rest) {
		t.Fatal("guard did not fire behind a global flag; the silent 1-of-N write survives")
	}
	if got := ClassifyVerb(verb, rest, false); got != Refuse {
		t.Fatalf("ClassifyVerb = %v, want Refuse", got)
	}
}
