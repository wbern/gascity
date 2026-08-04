package git

import (
	"fmt"
	"strings"
)

// HasUnpreservedCommitsResult reports whether removing this worktree would make
// any commit unreachable.
//
// This is a different question from HasUnlandedCommitsResult, and a strictly
// mechanical one. `git worktree remove` deletes the working directory and the
// worktree's admin files. It never deletes a branch, a tag, a remote-tracking
// ref, or any object those refs hold. So a commit that any durable ref contains
// survives removal, and only a commit reachable from nothing but this
// worktree's own detached HEAD is destroyed by it.
//
// That is the same reasoning this package already applies to stashes, which are
// deliberately not treated as a reason to hold a worktree: removal cannot
// destroy them either.
//
// Why the reaper needs this in addition to the landed probe: whether work
// LANDED is undecidable in the general case. A squash merge, a rebase, or a
// force-pushed correction rewrites the commits, and once the pull request's
// head is gone from the local repository no local comparison can recover the
// relationship at all. Whether work would be LOST, by contrast, is always
// decidable from refs alone, offline, with no provider lookup.
//
// It fails closed. Any probe error reports true — an unreadable repository is
// never assumed safe to remove.
func (g *Git) HasUnpreservedCommitsResult() (bool, error) {
	head, err := g.run("rev-parse", "--verify", "HEAD")
	if err != nil {
		return true, fmt.Errorf("resolving HEAD: %w", err)
	}
	head = strings.TrimSpace(head)
	if head == "" {
		return true, fmt.Errorf("resolving HEAD: empty result")
	}

	// Cheap path: a worktree on a branch is preserved by that branch, whatever
	// the branch tip is. Removal keeps the branch, so every commit the branch
	// reaches — HEAD included — outlives the worktree. This avoids the ref walk
	// below in the overwhelmingly common case.
	//
	// A detached HEAD has no symbolic ref, which is the only case that needs
	// the reachability scan.
	if _, err := g.run("symbolic-ref", "--quiet", "HEAD"); err == nil {
		return false, nil
	}

	// Detached: ask whether any durable ref contains this commit. Remote
	// -tracking refs count — the commit is published, so it is not this
	// worktree's to lose.
	out, err := g.run("for-each-ref", "--contains", head, "--format=%(refname)",
		"refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		return true, fmt.Errorf("scanning refs containing %s: %w", head, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			return false, nil
		}
	}
	return true, nil
}
