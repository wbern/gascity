package bdshim

import (
	"testing"

	"github.com/gastownhall/gascity/internal/bdflags"
)

// TestUpdatePositionalsKnowsEveryValueTakingFlag is the drift guard that was
// missing when the mistyped-pair guard shipped.
//
// Positional detection must consume the value of EVERY value-taking `bd update`
// flag. Using only the routable subset read the value of the other 26 as a
// positional issue id, so any `--flag key=value` looked like a dropped
// --set-metadata pair and was refused.
func TestUpdatePositionalsKnowsEveryValueTakingFlag(t *testing.T) {
	for flag := range bdflags.ValueFlags("update") {
		got := UpdatePositionals([]string{"gcw-1", flag, "role=worker"})
		if len(got) != 1 || got[0] != "gcw-1" {
			t.Errorf("UpdatePositionals(gcw-1 %s role=worker) = %v; the flag's value was read as an id", flag, got)
		}
	}
}

// TestUpdateGuardAllowsValueBearingFlags pins the shapes the guard broke. The
// first is shipped verbatim by the core skill pack
// (internal/bootstrap/packs/core/skills/gc-work/SKILL.md): `gc bd update <id>
// --add-label <key>=<value>`. It exited 1 with a refusal that told the caller to
// use --set-metadata instead — which writes metadata, not a label.
func TestUpdateGuardAllowsValueBearingFlags(t *testing.T) {
	for _, args := range [][]string{
		{"gcw-1", "--add-label", "role=worker"},
		{"gcw-1", "--set-labels", "a=b"},
		{"gcw-1", "--external-ref", "https://linear.app/i/ABC-1?tab=activity"},
		{"gcw-1", "--await-id", "run=12345"},
		{"gcw-1", "--session", "s=1"},
		{"gcw-1", "--unset-metadata", "k=v"},
		{"gcw-1", "--metadata", `{"url":"https://x?a=b"}`},
	} {
		if UpdateMistypedMetadataPair(args) {
			t.Errorf("UpdateMistypedMetadataPair(%v) = true; this is a valid invocation", args)
		}
		if got := ClassifyVerb("update", args, false); got == Refuse {
			t.Errorf("ClassifyVerb(update, %v) = Refuse; this is a valid invocation", args)
		}
	}
}

// TestUpdateGuardStillCatchesTheRealDefect pins that widening the flag table did
// not disarm the guard.
func TestUpdateGuardStillCatchesTheRealDefect(t *testing.T) {
	for _, args := range [][]string{
		{"gcw-1", "--set-metadata", "a=1", "b=2"},
		{"gcw-1", "--set-metadata", "a=1", "b=2", "c=3"},
		{"gcw-1", "--set-metadata=a=1", "b=2"},
	} {
		if !UpdateMistypedMetadataPair(args) {
			t.Errorf("UpdateMistypedMetadataPair(%v) = false; want the dropped pair caught", args)
		}
	}
}
