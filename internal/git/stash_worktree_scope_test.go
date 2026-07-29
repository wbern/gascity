package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stashRepoWide dirties the repo's seed file and stashes it, returning nothing:
// callers assert on the stash's visibility, not its content.
func stashRepoWide(t *testing.T, repo string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("stashed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "seed")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("edited\n"), 0o644); err != nil {
		t.Fatalf("dirty seed file: %v", err)
	}
	runGit(t, repo, "stash", "push", "-m", "wip")
	if !New(repo).HasStashes() {
		t.Fatal("fixture did not create a stash entry")
	}
}

// TestHasStashes_IsRepoWideNotWorktreeScoped pins the git behavior that makes
// per-worktree stash detection impossible: refs/stash lives in the common git
// dir, so a linked worktree that has never stashed anything still reports the
// repository's stashes.
//
// This is documentation-as-test. Callers that treat HasStashes as "this
// worktree has unsaved work" are wrong by construction, and anyone tempted to
// "fix" the reaper by making the probe worktree-scoped should fail here first:
// git offers no such scoping.
func TestHasStashes_IsRepoWideNotWorktreeScoped(t *testing.T) {
	repo := initTestRepo(t)
	stashRepoWide(t, repo)

	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, repo, "worktree", "add", "-b", "linked-branch", linked)

	if !New(linked).HasStashes() {
		t.Fatal("HasStashes() = false in a linked worktree, want true: refs/stash is repo-wide, " +
			"so the probe cannot distinguish this worktree's stashes from the repository's")
	}
	// The mechanism, asserted directly: refs/stash resolves into the shared
	// common dir from inside the linked worktree, not into its private git dir.
	stashRef, err := New(linked).run("rev-parse", "--path-format=absolute", "--git-path", "refs/stash")
	if err != nil {
		t.Fatalf("rev-parse --git-path refs/stash: %v", err)
	}
	commonDir, err := New(linked).run("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		t.Fatalf("rev-parse --git-common-dir: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(stashRef), strings.TrimSpace(commonDir)) {
		t.Errorf("refs/stash = %q, want it under the common git dir %q", stashRef, commonDir)
	}
}

// TestWorktreeRemove_PreservesStashes is the property that justifies demoting
// the reaper's stash gate from a blocker to a warning: removing a linked
// worktree cannot destroy a stash, because the stash lives in the common git
// dir that outlives the worktree.
func TestWorktreeRemove_PreservesStashes(t *testing.T) {
	repo := initTestRepo(t)
	stashRepoWide(t, repo)

	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, repo, "worktree", "add", "-b", "linked-branch", linked)

	if err := New(repo).WorktreeRemove(linked, false); err != nil {
		t.Fatalf("WorktreeRemove: %v", err)
	}
	if _, err := os.Stat(linked); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still present after removal (stat err=%v)", linked, err)
	}
	if !New(repo).HasStashes() {
		t.Fatal("stash disappeared after removing an unrelated worktree, want it preserved")
	}
	// The stash must still be applicable, not merely listed.
	if err := New(repo).StashPop(); err != nil {
		t.Fatalf("StashPop after worktree removal: %v", err)
	}
	if !New(repo).HasUncommittedWork() {
		t.Fatal("popped stash restored no changes, want the stashed edit back")
	}
}
