package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// stashRepoWide creates one stash entry in the rig's main working tree. Because
// refs/stash lives in the common git dir, that entry is visible from every
// linked worktree of the repository — which is precisely the production
// condition the rest of the reaper suite's fixtures never reproduce.
func stashRepoWide(t *testing.T, rigRoot string) {
	t.Helper()
	seed := filepath.Join(rigRoot, "README.md")
	if err := os.WriteFile(seed, []byte("stashed edit\n"), 0o644); err != nil {
		t.Fatalf("dirty seed file: %v", err)
	}
	mustGit(t, rigRoot, "stash", "push", "-m", "unrelated older work")
	if !git.New(rigRoot).HasStashes() {
		t.Fatal("fixture did not create a stash entry")
	}
}

// TestReapClosedBeadWorktrees_ReapsWhenRepoHasUnrelatedStash is the failing
// test for the whole defect: a closed-bead worktree that is clean, pushed and
// idle must still be reaped when the *repository* holds an unrelated stash.
//
// refs/stash resolves into the common git dir, so `git stash list` run inside a
// linked worktree reports every stash in the repo — none of which the worktree
// owns and none of which its removal can destroy. Gating the reap on that
// signal blocks 100% of candidates in any repo that has ever stashed (measured
// live: crm 58 stashes, every task worktree reports all 58, 0 of 61 closed-bead
// candidates passed the gate).
func TestReapClosedBeadWorktrees_ReapsWhenRepoHasUnrelatedStash(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	stashRepoWide(t, rigRoot)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-stsh01")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-stsh01", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-stsh01" {
		t.Fatalf("Reaped = %+v, want exactly ga-stsh01 despite the repo-wide stash\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still present after reap (stat err=%v)", wt, err)
	}
}

// TestReapClosedBeadWorktrees_ReapRecordsRepoWideStashWarning proves the
// downgrade is a downgrade and not a deletion: the stash signal is still
// observed and surfaced on the decision and on stderr, it just no longer
// blocks. Without this, dropping the gate would also drop the operator's only
// notice that stashed work exists somewhere in the repo.
func TestReapClosedBeadWorktrees_ReapRecordsRepoWideStashWarning(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	stashRepoWide(t, rigRoot)
	addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-stsh02")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-stsh02", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 1 {
		t.Fatalf("Reaped = %+v, want 1\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if w := report.Reaped[0].Warning; !strings.Contains(w, "stash") {
		t.Errorf("Reaped[0].Warning = %q, want it to mention the repo-wide stash", w)
	}
	if !strings.Contains(stderr.String(), "stash") {
		t.Errorf("stderr = %q, want a repo-wide stash warning line", stderr.String())
	}
}

// TestReapClosedBeadWorktrees_ProtectsUncommittedWorkDespiteStashDowngrade is
// the over-correction guard. Uncommitted work lives in the worktree's own
// index and working files, so removal *does* destroy it — that gate must
// survive the stash downgrade untouched. This test passes both before and
// after the fix; if it ever fails, the fix went too far.
func TestReapClosedBeadWorktrees_ProtectsUncommittedWorkDespiteStashDowngrade(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	stashRepoWide(t, rigRoot)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-stsh03")
	if err := os.WriteFile(filepath.Join(wt, "in-progress.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatalf("write uncommitted file: %v", err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-stsh03", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 for a worktree with uncommitted work", report.Reaped)
	}
	if len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, "uncommitted=true") {
		t.Fatalf("Protected = %+v, want 1 entry citing uncommitted work", report.Protected)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree with uncommitted work was removed: %v", err)
	}
}

// TestReapClosedBeadWorktrees_ProtectsUnpushedCommitsDespiteStashDowngrade is
// the second over-correction guard: unpushed commits are worktree-local
// history that removal would strand, so that gate must also survive.
func TestReapClosedBeadWorktrees_ProtectsUnpushedCommitsDespiteStashDowngrade(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	stashRepoWide(t, rigRoot)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-stsh04")
	if err := os.WriteFile(filepath.Join(wt, "done.txt"), []byte("finished\n"), 0o644); err != nil {
		t.Fatalf("write file to commit: %v", err)
	}
	mustGit(t, wt, "add", ".")
	mustGit(t, wt, "-c", "commit.gpgsign=false", "commit", "-m", "unpushed work")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-stsh04", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 for a worktree with unpushed commits", report.Reaped)
	}
	if len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, "unpushed=true") {
		t.Fatalf("Protected = %+v, want 1 entry citing unpushed commits", report.Protected)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree with unpushed commits was removed: %v", err)
	}
}

// injectReapGitProbe replaces the reaper's git-probe constructor for the
// duration of the test so probe *failures* — which real fixtures cannot
// produce on demand — are reachable. Paths other than the worktree under test
// get a benign fake so the pass completes.
//
// Path matching is symlink-normalized: the reaper discovers worktree paths from
// `git worktree list --porcelain`, which reports them resolved (on macOS a
// t.TempDir() under /var arrives as /private/var), so a raw string comparison
// against the fixture's own path would silently never match.
func injectReapGitProbe(t *testing.T, worktreePath string, probe gitProbe) {
	t.Helper()
	want := pathutil.NormalizePathForCompare(worktreePath)
	prev := newGitProbe
	newGitProbe = func(dir string) gitProbe {
		if pathutil.NormalizePathForCompare(dir) == want {
			return probe
		}
		return &fakeGitProbe{isRepo: true}
	}
	t.Cleanup(func() { newGitProbe = prev })
}

// TestReapClosedBeadWorktrees_ProtectsWhenUnpushedProbeFails closes the
// fail-open hole next to the stash gate. The reaper's contract says every git
// gate fails closed toward keeping the worktree, but it read the probes with
// `hasUnpushed, _ :=` — and HasUnpushedCommitsResult returns false alongside
// its error, so an unreadable repository was classified as safe to delete.
func TestReapClosedBeadWorktrees_ProtectsWhenUnpushedProbeFails(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-stsh05")
	injectReapGitProbe(t, wt, &fakeGitProbe{isRepo: true, unpushedErr: errors.New("git log exploded")})
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-stsh05", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 when the unpushed probe fails (fail closed)", report.Reaped)
	}
	if len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, "unpushed") {
		t.Fatalf("Protected = %+v, want 1 entry citing the failed unpushed probe", report.Protected)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree removed despite an indeterminate unpushed probe: %v", err)
	}
}

// TestReapClosedBeadWorktrees_ReapsWhenStashProbeFails is the deliberate
// asymmetry, and it is the reason the two probes are handled differently: once
// a repo-wide stash cannot endanger this worktree, neither can an unreadable
// stash list. The failure is recorded as a warning and the reap proceeds,
// rather than becoming a second permanent blocker.
func TestReapClosedBeadWorktrees_ReapsWhenStashProbeFails(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-stsh06")
	probe := &fakeGitProbe{isRepo: true, stashesErr: errors.New("git stash list exploded")}
	injectReapGitProbe(t, wt, probe)
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-stsh06", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"mrig": store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 1 {
		t.Fatalf("Reaped = %+v, want 1 when only the stash probe failed\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if w := report.Reaped[0].Warning; !strings.Contains(w, "stash") {
		t.Errorf("Reaped[0].Warning = %q, want it to record the failed stash probe", w)
	}
}
