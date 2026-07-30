package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// The Go closed-bead reaper was reachable only from the controller tick, behind
// a config flag. No operator and no order could ask it what it would do, which
// is why a bash reimplementation grew alongside it and why "enabling the reaper
// reaps zero" went unnoticed for weeks. These tests cover the verb that makes it
// answerable: wiring (which mode the reaper is invoked in) and rendering (what
// the operator reads back). The reaper's own gate behavior is not retested here
// — it has its own 23 tests.

// reapCmdHarness captures how doWorktreeReap invoked the reaper and lets a test
// hand back a fixed report.
type reapCmdHarness struct {
	called      int
	gotDryRun   bool
	gotCityPath string
	gotLiveDirs []string
	report      reapReport
}

// installReapCmdHarness redirects every collaborator doWorktreeReap depends on,
// so the test exercises the command's own logic rather than a real city.
func installReapCmdHarness(t *testing.T, h *reapCmdHarness) {
	t.Helper()
	cityPath := t.TempDir()
	h.gotCityPath = ""

	origResolve, origLoad, origReap, origStore, origLive := worktreeResolveCity, worktreeLoadCityConfig, worktreeReapClosedBeadWorktrees,
		worktreeOpenRigStore, worktreeLiveWorkerDirsFn
	t.Cleanup(func() {
		worktreeResolveCity, worktreeLoadCityConfig, worktreeReapClosedBeadWorktrees,
			worktreeOpenRigStore, worktreeLiveWorkerDirsFn = origResolve, origLoad, origReap, origStore, origLive
	})

	worktreeResolveCity = func() (string, error) { return cityPath, nil }
	worktreeLoadCityConfig = func(string, ...io.Writer) (*config.City, error) {
		return &config.City{Rigs: []config.Rig{{Name: "mrig", Path: t.TempDir()}}}, nil
	}
	worktreeOpenRigStore = func(string, string) (beads.Store, error) {
		return beads.NewMemStoreFrom(1, nil, nil), nil
	}
	worktreeLiveWorkerDirsFn = func(string) (map[string]bool, error) {
		return map[string]bool{"/live/one": true}, nil
	}
	worktreeReapClosedBeadWorktrees = func(cp string, _ *config.City, _ map[string]beads.Store, liveDirs []string, dryRun bool, _ events.Recorder, _ io.Writer) reapReport {
		h.called++
		h.gotDryRun = dryRun
		h.gotCityPath = cp
		h.gotLiveDirs = liveDirs
		rep := h.report
		rep.DryRun = dryRun
		return rep
	}
}

func sampleReapReport() reapReport {
	return reapReport{
		Reaped: []reapDecision{{
			BeadID: "ga-aaa1", Path: "/city/.gc/worktrees/mrig/builder/ga-aaa1", Rig: "mrig",
			Branch: "wt-ga-aaa1",
			Warning: "repository holds stashed work (repo-wide; not owned by this worktree " +
				"and not destroyed by its removal)",
		}},
		Protected: []reapDecision{{
			BeadID: "ga-bbb2", Path: "/city/.gc/worktrees/mrig/builder/ga-bbb2", Rig: "mrig",
			Reason: "unsafe git state: uncommitted=true unpushed=false",
		}},
	}
}

// TestDoWorktreeReap_DefaultsToDryRun is the safety default: a verb that can
// delete 300 worktrees must not do so because someone omitted a flag.
func TestDoWorktreeReap_DefaultsToDryRun(t *testing.T) {
	h := &reapCmdHarness{report: sampleReapReport()}
	installReapCmdHarness(t, h)

	var stdout, stderr bytes.Buffer
	if got := doWorktreeReap(nil, false, &stdout, &stderr); got != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", got, stderr.String())
	}
	if h.called != 1 {
		t.Fatalf("reaper called %d times, want 1", h.called)
	}
	if !h.gotDryRun {
		t.Error("reaper invoked with dryRun=false by default; the default must never delete")
	}
	if !strings.Contains(stdout.String(), "dry-run") {
		t.Errorf("stdout = %q, want it to say the run was a dry-run", stdout.String())
	}
}

// TestDoWorktreeReap_ExecuteOptsIntoRemoval proves the opt-in works at all —
// otherwise the verb would be permanently report-only and the bash reaper could
// never be retired.
func TestDoWorktreeReap_ExecuteOptsIntoRemoval(t *testing.T) {
	h := &reapCmdHarness{report: sampleReapReport()}
	installReapCmdHarness(t, h)

	var stdout, stderr bytes.Buffer
	if got := doWorktreeReap(nil, true, &stdout, &stderr); got != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", got, stderr.String())
	}
	if h.gotDryRun {
		t.Error("reaper invoked with dryRun=true under --execute")
	}
}

// TestDoWorktreeReap_RendersReasonsAndWarnings is the point of the verb: the
// operator must be able to read why each tree was kept, and see the repo-wide
// stash note that is no longer a blocker but still worth knowing.
func TestDoWorktreeReap_RendersReasonsAndWarnings(t *testing.T) {
	h := &reapCmdHarness{report: sampleReapReport()}
	installReapCmdHarness(t, h)

	var stdout, stderr bytes.Buffer
	doWorktreeReap(nil, false, &stdout, &stderr)

	out := stdout.String()
	for _, want := range []string{
		"ga-aaa1",                // the would-reap bead
		"ga-bbb2",                // the protected bead
		"uncommitted=true",       // why it was protected
		"stashed work",           // the non-blocking warning
		"1 would reap", "1 kept", // the summary
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestDoWorktreeReap_PassesLiveSessionDirs guards the gate that prevented the
// would-reap-19-live incident: the liveness cross-check is only as good as the
// live-session set handed to it, and a CLI caller must not pass an empty one.
func TestDoWorktreeReap_PassesLiveSessionDirs(t *testing.T) {
	h := &reapCmdHarness{report: reapReport{}}
	installReapCmdHarness(t, h)

	var stdout, stderr bytes.Buffer
	doWorktreeReap(nil, false, &stdout, &stderr)

	if len(h.gotLiveDirs) != 1 || h.gotLiveDirs[0] != "/live/one" {
		t.Errorf("liveSessionDirs = %v, want the live worker-dir set to reach the reaper", h.gotLiveDirs)
	}
}

// TestDoWorktreeReap_RejectsUnexpectedArgs keeps a typo from being read as
// consent to something else.
func TestDoWorktreeReap_RejectsUnexpectedArgs(t *testing.T) {
	h := &reapCmdHarness{report: reapReport{}}
	installReapCmdHarness(t, h)

	var stdout, stderr bytes.Buffer
	if got := doWorktreeReap([]string{"stray-arg"}, false, &stdout, &stderr); got == 0 {
		t.Fatal("exit = 0 for unexpected args, want non-zero")
	}
	if h.called != 0 {
		t.Error("reaper invoked despite bad arguments")
	}
}

// TestDoWorktreeReap_EmptyReportIsStated so "nothing happened" is never
// ambiguous with "the command did not run" — the exact confusion that cost a
// session of archeology on the reconciler prune.
func TestDoWorktreeReap_EmptyReportIsStated(t *testing.T) {
	h := &reapCmdHarness{report: reapReport{}}
	installReapCmdHarness(t, h)

	var stdout, stderr bytes.Buffer
	doWorktreeReap(nil, false, &stdout, &stderr)

	if out := stdout.String(); !strings.Contains(out, "0 would reap") && !strings.Contains(out, "No ") {
		t.Errorf("stdout = %q, want an explicit statement that nothing was found", out)
	}
}

// TestWorktreeReapCmd_IsRegisteredAndDefaultsToDryRunThroughCobra exercises the
// command the way a shell does, not the way a unit test finds convenient.
//
// This test exists because its absence hid a real defect. The sibling --json
// flag on this command family passed its unit tests for its entire life while
// failing on every real invocation: the tests called doWorktreeScan directly and
// so never met the CLI's JSON-contract gate, which rejects any command lacking a
// checked-in schema. A verb is only wired if it is wired through cobra.
func TestWorktreeReapCmd_IsRegisteredAndDefaultsToDryRunThroughCobra(t *testing.T) {
	h := &reapCmdHarness{report: sampleReapReport()}
	installReapCmdHarness(t, h)

	var stdout, stderr bytes.Buffer
	cmd := newWorktreeCmd(&stdout, &stderr)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"reap"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc worktree reap: %v\nstderr:\n%s", err, stderr.String())
	}
	if h.called != 1 {
		t.Fatalf("reaper called %d times via cobra, want 1", h.called)
	}
	if !h.gotDryRun {
		t.Error("cobra path invoked the reaper with dryRun=false by default")
	}

	// --execute must be a real, parsed flag rather than an unknown-flag error.
	h.called, h.gotDryRun = 0, true
	cmd = newWorktreeCmd(&stdout, &stderr)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"reap", "--execute"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc worktree reap --execute: %v", err)
	}
	if h.called != 1 || h.gotDryRun {
		t.Errorf("--execute via cobra: called=%d dryRun=%v, want 1 and false", h.called, h.gotDryRun)
	}
}
