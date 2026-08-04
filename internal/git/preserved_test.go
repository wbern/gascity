package git

import (
	"os"
	"testing"
)

// The preservation probe answers a different question from the landed probe:
// not "did this work reach a trunk" but "would removing this worktree make any
// commit unreachable". `git worktree remove` deletes the working directory and
// the worktree admin files; it never deletes refs or the objects they hold. So
// a commit any durable ref contains survives removal, exactly as a stash does.

func TestHasUnpreservedCommits_BranchBackedWorkSurvivesRemoval(t *testing.T) {
	clone := newTrunkRepo(t)

	// Committed on a branch and never pushed. The branch ref outlives the
	// worktree, so nothing here can be lost by removing the worktree.
	runGit(t, clone, "checkout", "-b", "feature")
	writeFile(t, clone, "work.txt", "unpushed but branch-backed\n")
	runGit(t, clone, "add", "work.txt")
	runGit(t, clone, "commit", "-m", "unpushed work")

	g := New(clone)
	got, err := g.HasUnpreservedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnpreservedCommitsResult() error = %v", err)
	}
	if got {
		t.Error("HasUnpreservedCommitsResult() = true for a branch-backed commit, want false (the branch survives removal)")
	}
}

func TestHasUnpreservedCommits_DetachedHeadWithNoCoveringRef(t *testing.T) {
	clone := newTrunkRepo(t)

	// A commit made on a detached HEAD, reachable from no ref at all. This is
	// the only shape whose commits removal genuinely destroys.
	runGit(t, clone, "checkout", "--detach")
	writeFile(t, clone, "orphan.txt", "reachable only from HEAD\n")
	runGit(t, clone, "add", "orphan.txt")
	runGit(t, clone, "commit", "-m", "orphan work")

	g := New(clone)
	got, err := g.HasUnpreservedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnpreservedCommitsResult() error = %v", err)
	}
	if !got {
		t.Error("HasUnpreservedCommitsResult() = false for a detached commit no ref contains, want true")
	}
}

func TestHasUnpreservedCommits_DetachedHeadCoveredByBranch(t *testing.T) {
	clone := newTrunkRepo(t)

	runGit(t, clone, "checkout", "-b", "keeper")
	writeFile(t, clone, "kept.txt", "kept by a branch\n")
	runGit(t, clone, "add", "kept.txt")
	runGit(t, clone, "commit", "-m", "kept work")
	head := gitOut(t, clone, "rev-parse", "HEAD")

	// Detach at that same commit. The branch still contains it, so removal
	// loses nothing even though HEAD is detached.
	runGit(t, clone, "checkout", "--detach", head)

	g := New(clone)
	got, err := g.HasUnpreservedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnpreservedCommitsResult() error = %v", err)
	}
	if got {
		t.Error("HasUnpreservedCommitsResult() = true for a detached HEAD a branch contains, want false")
	}
}

func TestHasUnpreservedCommits_DetachedHeadCoveredByTag(t *testing.T) {
	clone := newTrunkRepo(t)

	runGit(t, clone, "checkout", "--detach")
	writeFile(t, clone, "tagged.txt", "kept by a tag\n")
	runGit(t, clone, "add", "tagged.txt")
	runGit(t, clone, "commit", "-m", "tagged work")
	// update-ref rather than `git tag`: the ambient user config can force
	// annotated tags, which turns a bare `git tag <name>` into a failure and
	// would silently leave the fixture without the ref it is testing.
	runGit(t, clone, "update-ref", "refs/tags/keepsake", "HEAD")

	g := New(clone)
	got, err := g.HasUnpreservedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnpreservedCommitsResult() error = %v", err)
	}
	if got {
		t.Error("HasUnpreservedCommitsResult() = true for a detached HEAD a tag contains, want false")
	}
}

func TestHasUnpreservedCommits_DetachedHeadCoveredByRemoteRef(t *testing.T) {
	clone := newTrunkRepo(t)

	// Detached at the pushed trunk tip: the remote-tracking ref contains it.
	head := gitOut(t, clone, "rev-parse", "HEAD")
	runGit(t, clone, "checkout", "--detach", head)

	g := New(clone)
	got, err := g.HasUnpreservedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnpreservedCommitsResult() error = %v", err)
	}
	if got {
		t.Error("HasUnpreservedCommitsResult() = true for a detached HEAD origin/main contains, want false")
	}
}

func TestHasUnpreservedCommits_DetachedHeadAncestorOfBranch(t *testing.T) {
	clone := newTrunkRepo(t)

	runGit(t, clone, "checkout", "-b", "ahead")
	writeFile(t, clone, "first.txt", "first\n")
	runGit(t, clone, "add", "first.txt")
	runGit(t, clone, "commit", "-m", "first")
	first := gitOut(t, clone, "rev-parse", "HEAD")
	writeFile(t, clone, "second.txt", "second\n")
	runGit(t, clone, "add", "second.txt")
	runGit(t, clone, "commit", "-m", "second")

	// Detach at an ANCESTOR of the branch tip. --contains must see it.
	runGit(t, clone, "checkout", "--detach", first)

	g := New(clone)
	got, err := g.HasUnpreservedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnpreservedCommitsResult() error = %v", err)
	}
	if got {
		t.Error("HasUnpreservedCommitsResult() = true for a detached HEAD that is an ancestor of a branch, want false")
	}
}

func TestHasUnpreservedCommits_FailsClosedOnUnreadableRepo(t *testing.T) {
	dir := t.TempDir()
	// Not a git repository at all: every probe must report an error rather
	// than a reassuring false.
	g := New(dir)
	got, err := g.HasUnpreservedCommitsResult()
	if err == nil {
		t.Fatal("HasUnpreservedCommitsResult() error = nil for a non-repository, want an error")
	}
	if !got {
		t.Error("HasUnpreservedCommitsResult() = false on error, want true (fail closed)")
	}
}

func TestHasUnpreservedCommits_FailsClosedOnEmptyRepo(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "--initial-branch=main")
	// A repository with no commits: HEAD does not resolve. Nothing can be
	// lost, but the probe must not claim to have established that.
	g := New(dir)
	got, err := g.HasUnpreservedCommitsResult()
	if err == nil {
		t.Fatal("HasUnpreservedCommitsResult() error = nil for a repo with no commits, want an error")
	}
	if !got {
		t.Error("HasUnpreservedCommitsResult() = false on error, want true (fail closed)")
	}
}

func TestHasUnpreservedCommits_BranchRefAheadOfDetachedHead(t *testing.T) {
	// Regression guard for the cheap path: a worktree ON a branch is preserved
	// by that branch even when the branch has moved on beyond HEAD.
	clone := newTrunkRepo(t)
	runGit(t, clone, "checkout", "-b", "moving")
	writeFile(t, clone, "a.txt", "a\n")
	runGit(t, clone, "add", "a.txt")
	runGit(t, clone, "commit", "-m", "a")

	g := New(clone)
	got, err := g.HasUnpreservedCommitsResult()
	if err != nil {
		t.Fatalf("HasUnpreservedCommitsResult() error = %v", err)
	}
	if got {
		t.Error("HasUnpreservedCommitsResult() = true while on a branch, want false")
	}
	if _, err := os.Stat(clone); err != nil {
		t.Fatalf("worktree vanished during probe: %v", err)
	}
}
