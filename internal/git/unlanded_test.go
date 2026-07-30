package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitOut runs a git command in dir and returns its trimmed stdout, failing
// the test on error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = sanitizeGitEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// runGitAllowFail runs a git command in dir and ignores a non-zero exit, for
// setup steps whose precondition may legitimately be absent.
func runGitAllowFail(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = sanitizeGitEnv(os.Environ())
	_ = cmd.Run()
}

// newTrunkRepo returns a clone of a fresh bare remote with one pushed commit
// on a "main" trunk. The clone is configured for committing.
func newTrunkRepo(t *testing.T) string {
	t.Helper()
	const trunk = "main"
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare", "--initial-branch="+trunk)

	clone := t.TempDir()
	runGit(t, clone, "clone", bare, ".")
	runGit(t, clone, "config", "user.email", "test@test.com")
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "checkout", "-b", trunk)
	writeFile(t, clone, "base.txt", "base\n")
	runGit(t, clone, "add", "base.txt")
	runGit(t, clone, "commit", "-m", "base")
	runGit(t, clone, "push", "-u", "origin", trunk)
	return clone
}

// writeFile writes content into dir/name, failing the test on error.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

func TestHasUnlandedCommits_NoneWhenFullyPushed(t *testing.T) {
	clone := newTrunkRepo(t)

	g := New(clone)
	got, err := g.HasUnlandedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnlandedCommitsResult() error = %v", err)
	}
	if got {
		t.Error("HasUnlandedCommitsResult() = true for fully-pushed repo, want false")
	}
}

func TestHasUnlandedCommits_DetectsStrandedLocalCommit(t *testing.T) {
	clone := newTrunkRepo(t)

	// A commit whose content exists nowhere else. Removing this worktree
	// would destroy it.
	runGit(t, clone, "checkout", "-b", "feature")
	writeFile(t, clone, "stranded.txt", "work that landed nowhere\n")
	runGit(t, clone, "add", "stranded.txt")
	runGit(t, clone, "commit", "-m", "stranded work")

	g := New(clone)
	got, err := g.HasUnlandedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnlandedCommitsResult() error = %v", err)
	}
	if !got {
		t.Error("HasUnlandedCommitsResult() = false for a commit carried by no remote, want true")
	}
}

// TestHasUnlandedCommits_SquashMergedIsLanded is the case the plain
// reachability probe can never release: the work landed on the trunk under a
// different SHA, so HEAD is unreachable from every remote forever.
func TestHasUnlandedCommits_SquashMergedIsLanded(t *testing.T) {
	clone := newTrunkRepo(t)

	runGit(t, clone, "checkout", "-b", "feature")
	writeFile(t, clone, "feature.txt", "the feature\n")
	runGit(t, clone, "add", "feature.txt")
	runGit(t, clone, "commit", "-m", "add the feature")

	// The same diff lands on the trunk under a different SHA, exactly as a
	// squash-merge produces.
	runGit(t, clone, "checkout", "main")
	writeFile(t, clone, "feature.txt", "the feature\n")
	runGit(t, clone, "add", "feature.txt")
	runGit(t, clone, "commit", "-m", "add the feature (squashed from #1)")
	runGit(t, clone, "push", "origin", "main")
	runGit(t, clone, "fetch", "origin")
	runGit(t, clone, "checkout", "feature")

	g := New(clone)

	// Precondition: the plain reachability probe still calls this unpushed.
	// Without it the test could pass for the wrong reason.
	unpushed, err := g.HasUnpushedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnpushedCommitsResult() error = %v", err)
	}
	if !unpushed {
		t.Fatal("precondition failed: squash-merged HEAD is reachable from a remote, so this fixture no longer models the bug")
	}

	got, err := g.HasUnlandedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnlandedCommitsResult() error = %v", err)
	}
	if got {
		t.Error("HasUnlandedCommitsResult() = true for a squash-merged commit whose patch is on the trunk, want false")
	}
}

func TestHasUnlandedCommits_CherryPickedIsLanded(t *testing.T) {
	clone := newTrunkRepo(t)

	runGit(t, clone, "checkout", "-b", "feature")
	writeFile(t, clone, "picked.txt", "picked content\n")
	runGit(t, clone, "add", "picked.txt")
	runGit(t, clone, "commit", "-m", "work to be picked")
	sha := gitOut(t, clone, "rev-parse", "HEAD")

	// The trunk must move first. Cherry-picking onto the very parent the
	// commit already has reproduces an identical commit object — same SHA —
	// which would make this fixture pass without the patch-equivalence rule.
	runGit(t, clone, "checkout", "main")
	writeFile(t, clone, "trunk-moved.txt", "trunk moved on\n")
	runGit(t, clone, "add", "trunk-moved.txt")
	runGit(t, clone, "commit", "-m", "unrelated trunk work")
	runGit(t, clone, "cherry-pick", sha)
	runGit(t, clone, "push", "origin", "main")
	runGit(t, clone, "fetch", "origin")
	runGit(t, clone, "checkout", "feature")

	g := New(clone)

	unpushed, err := g.HasUnpushedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnpushedCommitsResult() error = %v", err)
	}
	if !unpushed {
		t.Fatal("precondition failed: cherry-picked HEAD is reachable from a remote, so this fixture no longer models the bug")
	}

	got, err := g.HasUnlandedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnlandedCommitsResult() error = %v", err)
	}
	if got {
		t.Error("HasUnlandedCommitsResult() = true for a cherry-picked commit already on the trunk, want false")
	}
}

// TestHasUnlandedCommits_MixedLandedAndStranded guards the whole-set rule: one
// unlanded commit among landed ones must hold the worktree.
func TestHasUnlandedCommits_MixedLandedAndStranded(t *testing.T) {
	clone := newTrunkRepo(t)

	runGit(t, clone, "checkout", "-b", "feature")
	writeFile(t, clone, "landed.txt", "landed\n")
	runGit(t, clone, "add", "landed.txt")
	runGit(t, clone, "commit", "-m", "landed work")
	writeFile(t, clone, "stranded.txt", "stranded\n")
	runGit(t, clone, "add", "stranded.txt")
	runGit(t, clone, "commit", "-m", "stranded work")

	// Only the first commit's patch reaches the trunk.
	runGit(t, clone, "checkout", "main")
	writeFile(t, clone, "landed.txt", "landed\n")
	runGit(t, clone, "add", "landed.txt")
	runGit(t, clone, "commit", "-m", "landed work (squashed)")
	runGit(t, clone, "push", "origin", "main")
	runGit(t, clone, "fetch", "origin")
	runGit(t, clone, "checkout", "feature")

	g := New(clone)
	got, err := g.HasUnlandedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnlandedCommitsResult() error = %v", err)
	}
	if !got {
		t.Error("HasUnlandedCommitsResult() = false when one commit of the set landed nowhere, want true")
	}
}

// TestHasUnlandedCommits_ReachableFromNonTrunkRemote pins the no-regression
// rule: anything the reachability probe already calls safe stays safe, even
// when its patch never reached the trunk.
func TestHasUnlandedCommits_ReachableFromNonTrunkRemote(t *testing.T) {
	clone := newTrunkRepo(t)

	runGit(t, clone, "checkout", "-b", "feature")
	writeFile(t, clone, "pushed.txt", "pushed to a side branch\n")
	runGit(t, clone, "add", "pushed.txt")
	runGit(t, clone, "commit", "-m", "side branch work")
	runGit(t, clone, "push", "origin", "feature")
	runGit(t, clone, "fetch", "origin")

	g := New(clone)
	unpushed, err := g.HasUnpushedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnpushedCommitsResult() error = %v", err)
	}
	if unpushed {
		t.Fatal("precondition failed: commit pushed to origin/feature should be reachable from a remote")
	}

	got, err := g.HasUnlandedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnlandedCommitsResult() error = %v", err)
	}
	if got {
		t.Error("HasUnlandedCommitsResult() = true for a commit reachable from a non-trunk remote branch, want false (must not be stricter than the reachability probe)")
	}
}

// TestHasUnlandedCommits_UnpushedMergeFailsClosed pins the conservative rule
// for merge commits: patch-id equivalence cannot describe a merge, so a merge
// no remote carries holds the worktree.
func TestHasUnlandedCommits_UnpushedMergeFailsClosed(t *testing.T) {
	clone := newTrunkRepo(t)

	runGit(t, clone, "checkout", "-b", "side")
	writeFile(t, clone, "side.txt", "side\n")
	runGit(t, clone, "add", "side.txt")
	runGit(t, clone, "commit", "-m", "side work")

	runGit(t, clone, "checkout", "-b", "feature", "main")
	writeFile(t, clone, "feature.txt", "feature\n")
	runGit(t, clone, "add", "feature.txt")
	runGit(t, clone, "commit", "-m", "feature work")
	runGit(t, clone, "merge", "--no-ff", "-m", "merge side into feature", "side")

	// Both parents' patches land on the trunk as separate commits, so their
	// individual patch-ids match and only the merge itself is undescribable —
	// the one thing this rule exists for. Landing them as a single squashed
	// commit would produce a combined diff matching neither, and the test
	// would then pass on the unlanded parents rather than on the merge.
	runGit(t, clone, "checkout", "main")
	writeFile(t, clone, "side.txt", "side\n")
	runGit(t, clone, "add", "side.txt")
	runGit(t, clone, "commit", "-m", "side work (squashed)")
	writeFile(t, clone, "feature.txt", "feature\n")
	runGit(t, clone, "add", "feature.txt")
	runGit(t, clone, "commit", "-m", "feature work (squashed)")
	runGit(t, clone, "push", "origin", "main")
	runGit(t, clone, "fetch", "origin")
	runGit(t, clone, "checkout", "feature")

	g := New(clone)
	got, err := g.HasUnlandedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnlandedCommitsResult() error = %v", err)
	}
	if !got {
		t.Error("HasUnlandedCommitsResult() = false for an unpushed merge commit, want true (merges fail closed)")
	}
}

func TestHasUnlandedCommits_NoRemote(t *testing.T) {
	repo := initTestRepo(t)

	g := New(repo)
	got, err := g.HasUnlandedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnlandedCommitsResult() error = %v", err)
	}
	if !got {
		t.Error("HasUnlandedCommitsResult() = false for a repo with no remote, want true")
	}
}

// TestHasUnlandedCommits_NoTrunkRefFailsClosed covers a repo that has a remote
// carrying no recognizable trunk: there is nothing to prove the work landed
// against, so it must be held.
func TestHasUnlandedCommits_NoTrunkRefFailsClosed(t *testing.T) {
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare", "--initial-branch=release-2026")

	clone := t.TempDir()
	runGit(t, clone, "clone", bare, ".")
	runGit(t, clone, "config", "user.email", "test@test.com")
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "checkout", "-b", "release-2026")
	writeFile(t, clone, "base.txt", "base\n")
	runGit(t, clone, "add", "base.txt")
	runGit(t, clone, "commit", "-m", "base")
	runGit(t, clone, "push", "-u", "origin", "release-2026")
	// Some clones carry no origin/HEAD symref at all.
	runGitAllowFail(t, clone, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")

	writeFile(t, clone, "more.txt", "more\n")
	runGit(t, clone, "add", "more.txt")
	runGit(t, clone, "commit", "-m", "local work")

	g := New(clone)
	got, err := g.HasUnlandedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnlandedCommitsResult() error = %v", err)
	}
	if !got {
		t.Error("HasUnlandedCommitsResult() = false with no resolvable trunk, want true (fails closed)")
	}
}

func TestHasUnlandedCommitsResult_ReturnsProbeError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))

	g := New(dir)
	if _, err := g.HasUnlandedCommitsResult(); err == nil {
		t.Fatal("HasUnlandedCommitsResult() error = nil, want probe error")
	}
	if !g.HasUnlandedCommits() {
		t.Error("HasUnlandedCommits() should fail closed on probe errors")
	}
}
