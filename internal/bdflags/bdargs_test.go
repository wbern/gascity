package bdflags

import (
	"reflect"
	"testing"
)

// TestSplitGlobalFlagsSkipsGlobalFlagValues pins that a global flag's VALUE is
// never mistaken for the subcommand. Taking the first non-dash token reads
// "bob" out of `bd --actor bob update <id> ...`, so anything keyed off the
// subcommand is bypassed by a token the caller never meant as a verb.
func TestSplitGlobalFlagsSkipsGlobalFlagValues(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantVerb string
		wantRest []string
	}{
		{"plain", []string{"update", "bd-1"}, "update", []string{"bd-1"}},
		{"--actor", []string{"--actor", "bob", "update", "bd-1"}, "update", []string{"bd-1"}},
		{"-C dir", []string{"-C", "/some/dir", "update", "bd-1"}, "update", []string{"bd-1"}},
		{"--db", []string{"--db", "/x/y.db", "update", "bd-1"}, "update", []string{"bd-1"}},
		{"--directory", []string{"--directory", "/d", "close", "bd-1"}, "close", []string{"bd-1"}},
		{"inline form consumes nothing", []string{"--actor=bob", "update", "bd-1"}, "update", []string{"bd-1"}},
		{"bool global", []string{"--json", "update", "bd-1"}, "update", []string{"bd-1"}},
		{"stacked", []string{"--actor", "bob", "--json", "-C", "/d", "update", "bd-1"}, "update", []string{"bd-1"}},
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

// TestPositionalsKnowsEveryValueTakingFlag is the drift guard: positional
// detection must consume the value of EVERY value-taking flag for the
// subcommand. With a partial set, the value of any omitted flag is read as a
// positional — which is how `update <id> --add-label role=worker` came to look
// like a stray key=value token.
func TestPositionalsKnowsEveryValueTakingFlag(t *testing.T) {
	for flag := range ValueFlags("update") {
		got := Positionals("update", []string{"bd-1", flag, "role=worker"})
		if len(got) != 1 || got[0] != "bd-1" {
			t.Errorf("Positionals(update, bd-1 %s role=worker) = %v; the flag's value was read as an id", flag, got)
		}
	}
}

// TestDroppedMetadataPairs pins detection in both directions.
func TestDroppedMetadataPairs(t *testing.T) {
	dropped := [][]string{
		{"bd-1", "--set-metadata", "a=1", "b=2"},
		{"bd-1", "--set-metadata", "a=1", "b=2", "c=3"},
		{"bd-1", "--set-metadata=a=1", "b=2"},
	}
	for _, args := range dropped {
		if len(DroppedMetadataPairs(args)) == 0 {
			t.Errorf("DroppedMetadataPairs(%v) = none; want the dropped pair caught", args)
		}
	}
	valid := [][]string{
		{"bd-1", "--set-metadata", "a=1"},
		{"bd-1", "--set-metadata", "a=1", "--set-metadata", "b=2"},
		{"bd-1", "--add-label", "role=worker"},
		{"bd-1", "--set-labels", "a=b"},
		{"bd-1", "--external-ref", "https://example.test/i/ABC-1?tab=activity"},
		{"bd-1", "--metadata", `{"url":"https://x?a=b"}`},
		{"bd-1", "bd-2", "--set-metadata", "a=1"},
	}
	for _, args := range valid {
		if got := DroppedMetadataPairs(args); len(got) != 0 {
			t.Errorf("DroppedMetadataPairs(%v) = %v; this is a valid invocation", args, got)
		}
	}
}

// TestDroppedMetadataRefusalOnlyUpdate pins that the refusal is scoped to update.
func TestDroppedMetadataRefusalOnlyUpdate(t *testing.T) {
	if _, ok := DroppedMetadataRefusal("gc bd", "create", []string{"t", "--set-metadata", "a=1", "b=2"}); ok {
		t.Error("refusal fired for create; --set-metadata is an update flag")
	}
	msg, ok := DroppedMetadataRefusal("gc bd", "update", []string{"bd-1", "--set-metadata", "a=1", "b=2"})
	if !ok {
		t.Fatal("refusal did not fire for update")
	}
	for _, want := range []string{"b=2", "--set-metadata", "exits 0"} {
		if !contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()
}
