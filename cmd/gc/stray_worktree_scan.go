package main

import (
	"io/fs"
	"os"
	"path/filepath"
)

// strayWorktree is one git checkout found under a managed root that is not
// bound to a live session. Reclaimable is true only when the checkout probes
// clean (no uncommitted, unpushed, or stashed work); otherwise Reason records
// why it must be left alone. This is a report-only classification — gcw-noyd
// slice 1 never removes anything.
type strayWorktree struct {
	Path        string
	Reclaimable bool
	Reason      string
}

// scanStrayWorktrees walks each managed root and reports every git checkout
// (a directory containing a .git dir or file — a registered worktree OR an
// unregistered clone that `git worktree list` never sees) that is not the
// working directory of a live session.
//
// Detection is deliberately filesystem-level, not worktree-list-level, because
// the heaviest cruft is unregistered clones git does not track as worktrees.
// Reclaimability reuses the same clean/unpushed/stash gate as
// pruneAgentHomeWorktreeIfSafe, so the report and the (future, flag-gated)
// removal share one safety definition.
//
// A checkout found under a root is classified but not descended into, so nested
// checkouts inside a live worktree are out of scope for this slice (tracked
// separately). liveWorkerDirs holds cleaned absolute paths bound to live
// sessions; those are skipped and never classified. probeFor constructs a
// gitProbe for a directory (newGitProbe in production, a fake in tests).
func scanStrayWorktrees(roots []string, liveWorkerDirs map[string]bool, probeFor func(string) gitProbe) ([]strayWorktree, error) {
	var out []strayWorktree
	for _, root := range roots {
		root = filepath.Clean(root)
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// Unreadable entry — skip it rather than aborting the whole
				// scan; a report that stops at the first permission error is
				// worse than a report that omits one unreadable subtree.
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !d.IsDir() || path == root {
				return nil
			}
			if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr != nil {
				return nil // not a checkout; keep descending
			}
			// path is a git checkout. Classify unless it is a live session's
			// working directory, then stop descending into it either way.
			clean := filepath.Clean(path)
			if !liveWorkerDirs[clean] {
				out = append(out, classifyStrayWorktree(clean, probeFor))
			}
			return fs.SkipDir
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}
	return out, nil
}

// classifyStrayWorktree applies the same safety gate as
// pruneAgentHomeWorktreeIfSafe: a checkout is reclaimable only when it is a git
// repo with no uncommitted changes, no unpushed commits, and no stashes. A
// failed probe is treated as not-reclaimable — never guess in favor of removal.
func classifyStrayWorktree(path string, probeFor func(string) gitProbe) strayWorktree {
	gp := probeFor(path)
	if !gp.IsRepo() {
		return strayWorktree{Path: path, Reason: "not a git repo"}
	}
	if gp.HasUncommittedWork() {
		return strayWorktree{Path: path, Reason: "uncommitted changes"}
	}
	if unpushed, err := gp.HasUnpushedCommitsResult(); err != nil {
		return strayWorktree{Path: path, Reason: "unpushed probe failed"}
	} else if unpushed {
		return strayWorktree{Path: path, Reason: "unpushed commits"}
	}
	if stashes, err := gp.HasStashesResult(); err != nil {
		return strayWorktree{Path: path, Reason: "stash probe failed"}
	} else if stashes {
		return strayWorktree{Path: path, Reason: "stashed work"}
	}
	return strayWorktree{Path: path, Reclaimable: true}
}
