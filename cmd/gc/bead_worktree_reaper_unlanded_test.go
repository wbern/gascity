package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// TestReapClosedBeadWorktrees_FlagsUnlandedWork is the wiring guard for the
// summary count. Without it the renderer could report a field production never
// sets, and the count would read zero on a fleet full of stranded work.
//
// The flag describes the TREE ("its commits reached no trunk"), not the
// decision, so it must be set whichever way the decision goes. Here the commit
// sits on the worktree's branch, so removal cannot destroy it and the tree is
// reaped — and the count must still see the unlanded work.
func TestReapClosedBeadWorktrees_FlagsUnlandedWork(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-unl01")
	if err := os.WriteFile(filepath.Join(wt, "stranded.txt"), []byte("exists nowhere else\n"), 0o644); err != nil {
		t.Fatalf("write file to commit: %v", err)
	}
	mustGit(t, wt, "add", ".")
	mustGit(t, wt, "-c", "commit.gpgsign=false", "commit", "-m", "work carried by no remote")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-unl01", Status: "closed"}}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 1 {
		t.Fatalf("Reaped = %+v, want exactly 1 (the branch preserves the commit)", report.Reaped)
	}
	if !report.Reaped[0].HoldsUnlandedWork {
		t.Errorf("HoldsUnlandedWork = false for a worktree whose commit no remote carries; the summary count would silently read zero")
	}
}

// TestReapClosedBeadWorktrees_DirtyTreeIsNotUnlandedWork keeps the flag narrow:
// an uncommitted tree is protected too, but it is not the irreplaceable case
// and must not inflate the count.
func TestReapClosedBeadWorktrees_DirtyTreeIsNotUnlandedWork(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-unl02")
	if err := os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatalf("write uncommitted file: %v", err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-unl02", Status: "closed"}}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Protected) != 1 {
		t.Fatalf("Protected = %+v, want exactly 1", report.Protected)
	}
	if report.Protected[0].HoldsUnlandedWork {
		t.Errorf("HoldsUnlandedWork = true for a merely dirty tree (reason=%q); the count must stay specific to work no remote carries", report.Protected[0].Reason)
	}
}

// TestRenderReapReport_CountsUnlandedWorkSeparately pins the signal that made
// 47 worktrees of irreplaceable work invisible for three weeks: a summary
// reading "0 would reap, 120 kept" cannot distinguish a fleet that is merely
// busy from one holding commits that exist nowhere else.
func TestRenderReapReport_CountsUnlandedWorkSeparately(t *testing.T) {
	report := reapReport{
		DryRun: true,
		Reaped: []reapDecision{{BeadID: "b-1", Rig: "r", Path: "/wt/1"}},
		Protected: []reapDecision{
			{BeadID: "b-2", Rig: "r", Path: "/wt/2", Reason: "unsafe git state: uncommitted=false unlanded=true", HoldsUnlandedWork: true},
			{BeadID: "b-3", Rig: "r", Path: "/wt/3", Reason: "unsafe git state: uncommitted=true unlanded=false"},
			{BeadID: "b-4", Rig: "r", Path: "/wt/4", Reason: "live: live process cwd /wt/4"},
		},
	}

	var out bytes.Buffer
	renderReapReport(&out, report)

	got := out.String()
	if !strings.Contains(got, "1 would reap, 3 kept") {
		t.Errorf("summary lost its existing counts: %q", got)
	}
	if !strings.Contains(got, "1 holding unlanded work") {
		t.Errorf("summary = %q, want it to call out the worktrees holding work that exists nowhere else", got)
	}
}

// TestRenderReapReport_SilentWhenNothingStranded keeps the summary quiet in the
// ordinary case: a count that always prints is a count nobody reads.
func TestRenderReapReport_SilentWhenNothingStranded(t *testing.T) {
	report := reapReport{
		DryRun: true,
		Protected: []reapDecision{
			{BeadID: "b-3", Rig: "r", Path: "/wt/3", Reason: "unsafe git state: uncommitted=true unlanded=false"},
		},
	}

	var out bytes.Buffer
	renderReapReport(&out, report)

	if got := out.String(); strings.Contains(got, "unlanded work") {
		t.Errorf("summary = %q, want no stranded-work clause when none is held", got)
	}
}
