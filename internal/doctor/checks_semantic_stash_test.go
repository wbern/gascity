package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// classifyNested rejected any nested worktree whose repository held a stash. But
// refs/stash lives in the repository's COMMON git dir, so `git stash list` run
// inside a linked worktree reports every stash in the repo — none of which that
// worktree owns, and none of which its removal can destroy (proved by
// internal/git's TestWorktreeRemove_PreservesStashes). Measured live: crm holds
// 58 repo-wide stashes, visible from every one of its task worktrees.
//
// So doctor told the operator that every nested worktree was unsafe to remove,
// in any repository that had ever stashed. Same defect as the closed-bead
// reaper's (fixed in 14c9c78ca); this is the doctor half.

func nestedProbeFor(path string, stashed bool, stashErr error) func(string) gitWorktree {
	return func(p string) gitWorktree {
		return &fakeGitWorktree{
			currentPath: p,
			stashed:     map[string]bool{path: stashed},
			stashedErr:  map[string]error{path: stashErr},
		}
	}
}

func TestClassifyNested_SafeDespiteRepoWideStash(t *testing.T) {
	const path = "/city/.gc/worktrees/rig/home/nested"

	got := classifyNested(nestedProbeFor(path, true, nil), path, "/city/.gc/worktrees/rig/home", "wt-branch")

	if !got.safeToRm {
		t.Fatalf("safeToRm = false (reason %q), want true: a repo-wide stash is not this worktree's unsaved work", got.reason)
	}
	if got.probeErr {
		t.Error("probeErr = true for a successful stash probe")
	}
}

// TestClassifyNested_RecordsRepoWideStashWarning proves the signal is demoted
// rather than discarded — doctor still tells the operator stashed work exists
// somewhere in the repository.
func TestClassifyNested_RecordsRepoWideStashWarning(t *testing.T) {
	const path = "/city/.gc/worktrees/rig/home/nested"

	got := classifyNested(nestedProbeFor(path, true, nil), path, "/city/.gc/worktrees/rig/home", "wt-branch")

	if !strings.Contains(got.warning, "stash") {
		t.Errorf("warning = %q, want it to mention the repo-wide stash", got.warning)
	}
}

// TestClassifyNested_StashProbeFailureIsOnlyAWarning is the deliberate
// asymmetry. Doctor is right to fail closed on the uncommitted and unpushed
// probes — those describe work inside the worktree. A stash probe describes
// something removal cannot touch, so its failure must not make the worktree
// permanently unsafe.
func TestClassifyNested_StashProbeFailureIsOnlyAWarning(t *testing.T) {
	const path = "/city/.gc/worktrees/rig/home/nested"

	got := classifyNested(nestedProbeFor(path, false, errors.New("git stash list exploded")), path, "/city/.gc/worktrees/rig/home", "wt-branch")

	if !got.safeToRm {
		t.Errorf("safeToRm = false (reason %q), want true when only the stash probe failed", got.reason)
	}
	if got.probeErr {
		t.Error("probeErr = true for a failed STASH probe; that flag drives doctor's degraded-scan messaging and belongs to probes that gate safety")
	}
	if !strings.Contains(got.warning, "stash") {
		t.Errorf("warning = %q, want the failed stash probe recorded", got.warning)
	}
}

// TestClassifyNested_StillProtectsRealWork is the over-correction guard: the
// gates describing worktree-local work must survive untouched, including their
// fail-closed behavior on a probe error. Passes before and after the fix.
func TestClassifyNested_StillProtectsRealWork(t *testing.T) {
	const path = "/city/.gc/worktrees/rig/home/nested"
	cases := []struct {
		name         string
		probe        *fakeGitWorktree
		wantReason   string
		wantProbeErr bool
	}{
		{
			name:       "uncommitted changes alongside a repo-wide stash",
			probe:      &fakeGitWorktree{uncommitted: map[string]bool{path: true}, stashed: map[string]bool{path: true}},
			wantReason: "uncommitted",
		},
		{
			name:       "unpushed commits alongside a repo-wide stash",
			probe:      &fakeGitWorktree{unpushed: map[string]bool{path: true}, stashed: map[string]bool{path: true}},
			wantReason: "unpushed",
		},
		{
			name:         "unpushed probe failure still fails closed",
			probe:        &fakeGitWorktree{unpushedErr: map[string]error{path: errors.New("boom")}},
			wantReason:   "unpushed",
			wantProbeErr: true,
		},
		{
			name:       "not a git repo",
			probe:      &fakeGitWorktree{notRepo: map[string]bool{path: true}},
			wantReason: "git status unreadable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := tc.probe
			got := classifyNested(func(p string) gitWorktree { probe.currentPath = p; return probe },
				path, "/city/.gc/worktrees/rig/home", "wt-branch")
			if got.safeToRm {
				t.Fatalf("safeToRm = true, want false for %s", tc.name)
			}
			if !strings.Contains(got.reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", got.reason, tc.wantReason)
			}
			if got.probeErr != tc.wantProbeErr {
				t.Errorf("probeErr = %v, want %v", got.probeErr, tc.wantProbeErr)
			}
		})
	}
}

// TestNestedWorktreePruneCheck_SurfacesStashWarningInDetails closes the loop on
// the warning: a field only tests read is not an operator-visible signal. The
// repo-wide stash note must reach the details an operator reads before
// authorizing the fix.
func TestNestedWorktreePruneCheck_SurfacesStashWarningInDetails(t *testing.T) {
	dir := t.TempDir()
	home := makeAgentHome(t, dir, "agent-1")
	stashed := pathutil.NormalizePathForCompare(filepath.Join(home, "worktrees", "task-stashed"))
	if err := os.MkdirAll(stashed, 0o755); err != nil {
		t.Fatal(err)
	}

	c := &NestedWorktreePruneCheck{
		cfg: config.DoctorConfig{},
		newGit: func(path string) gitWorktree {
			return &fakeGitWorktree{
				listResp: []git.Worktree{
					{Path: home, Branch: "h"},
					{Path: stashed, Branch: "task-stashed"},
				},
				stashed:     map[string]bool{stashed: true},
				currentPath: path,
			}
		},
	}
	r := c.Run(&CheckContext{CityPath: dir})

	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, "stash") {
		t.Errorf("details = %q, want the repo-wide stash note surfaced to the operator", joined)
	}
}
