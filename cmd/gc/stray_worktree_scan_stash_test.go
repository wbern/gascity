package main

import (
	"errors"
	"strings"
	"testing"
)

// refs/stash lives in the repository's COMMON git dir, so `git stash list` run
// inside any linked worktree reports every stash in the repo — none of which
// that worktree owns, and none of which its removal can destroy (proved by
// internal/git's TestWorktreeRemove_PreservesStashes). Measured live: crm holds
// 58 repo-wide stashes and every one of its task worktrees reports all 58.
//
// Treating that as unsafe state marked every stray checkout unreclaimable in
// any repository that had ever stashed, which is the same defect already fixed
// in the closed-bead reaper (14c9c78ca). These tests cover the same fix here,
// including the guards that prove the gate was narrowed rather than removed.

// stashProbe is a gitProbe that can also fail its stash probe, which
// fakeStrayProbe cannot express (it hardcodes a nil error).
type stashProbe struct {
	fakeStrayProbe
	stashErr error
}

func (s stashProbe) HasStashesResult() (bool, error) { return s.stashes, s.stashErr }

func TestClassifyStrayWorktree_ReclaimableDespiteRepoWideStash(t *testing.T) {
	got := classifyStrayWorktree("/w/stray", func(string) gitProbe {
		return stashProbe{fakeStrayProbe: fakeStrayProbe{isRepo: true, stashes: true}}
	})

	if !got.Reclaimable {
		t.Fatalf("Reclaimable = false (reason %q), want true: a repo-wide stash is not this checkout's unsaved work", got.Reason)
	}
}

// TestClassifyStrayWorktree_RecordsRepoWideStashWarning proves the signal is
// demoted, not discarded — an operator still learns stashed work exists
// somewhere in the repo.
func TestClassifyStrayWorktree_RecordsRepoWideStashWarning(t *testing.T) {
	got := classifyStrayWorktree("/w/stray", func(string) gitProbe {
		return stashProbe{fakeStrayProbe: fakeStrayProbe{isRepo: true, stashes: true}}
	})

	if !strings.Contains(got.Warning, "stash") {
		t.Errorf("Warning = %q, want it to mention the repo-wide stash", got.Warning)
	}
}

// TestClassifyStrayWorktree_StashProbeFailureIsOnlyAWarning is the deliberate
// asymmetry: a signal that cannot endanger this checkout when readable cannot
// endanger it when unreadable, so a failed stash probe must not become a second
// permanent blocker.
func TestClassifyStrayWorktree_StashProbeFailureIsOnlyAWarning(t *testing.T) {
	got := classifyStrayWorktree("/w/stray", func(string) gitProbe {
		return stashProbe{fakeStrayProbe: fakeStrayProbe{isRepo: true}, stashErr: errors.New("git stash list exploded")}
	})

	if !got.Reclaimable {
		t.Errorf("Reclaimable = false (reason %q), want true when only the stash probe failed", got.Reason)
	}
	if !strings.Contains(got.Warning, "stash") {
		t.Errorf("Warning = %q, want the failed stash probe recorded", got.Warning)
	}
}

// TestClassifyStrayWorktree_StillProtectsRealWork is the over-correction guard:
// whatever removal DOES destroy must keep protecting the checkout.
//
// Two shapes qualify. Uncommitted changes live only in the working files.
// Commits qualify only when no ref carries them — a checkout sitting on a
// branch is not one of them, because removal deletes the directory and leaves
// the branch, so `unpushed` alone is not the condition. The commit case
// therefore sets unpreserved, which is what "removal destroys this" means.
func TestClassifyStrayWorktree_StillProtectsRealWork(t *testing.T) {
	cases := []struct {
		name       string
		probe      gitProbe
		wantReason string
	}{
		{
			name:       "uncommitted changes with a repo-wide stash present",
			probe:      stashProbe{fakeStrayProbe: fakeStrayProbe{isRepo: true, uncommitted: true, stashes: true}},
			wantReason: "uncommitted",
		},
		{
			name:       "unlanded commits no ref carries, with a repo-wide stash present",
			probe:      stashProbe{fakeStrayProbe: fakeStrayProbe{isRepo: true, unpushed: true, unpreserved: true, stashes: true}},
			wantReason: "unlanded",
		},
		{
			name:       "not a git repo",
			probe:      stashProbe{fakeStrayProbe: fakeStrayProbe{isRepo: false}},
			wantReason: "not a git repo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyStrayWorktree("/w/stray", func(string) gitProbe { return tc.probe })
			if got.Reclaimable {
				t.Fatalf("Reclaimable = true, want false for %s", tc.name)
			}
			if !strings.Contains(got.Reason, tc.wantReason) {
				t.Errorf("Reason = %q, want it to mention %q", got.Reason, tc.wantReason)
			}
		})
	}
}
