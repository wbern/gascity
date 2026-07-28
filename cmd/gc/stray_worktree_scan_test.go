package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// fakeStrayProbe is a gitProbe whose answers are keyed by the directory it is
// constructed for, so a single scan can classify several checkouts differently.
type fakeStrayProbe struct {
	isRepo      bool
	uncommitted bool
	unpushed    bool
	stashes     bool
	branch      string
}

func (f fakeStrayProbe) IsRepo() bool                            { return f.isRepo }
func (f fakeStrayProbe) CurrentBranch() (string, error)          { return f.branch, nil }
func (f fakeStrayProbe) HasUncommittedWork() bool                { return f.uncommitted }
func (f fakeStrayProbe) HasUnpushedCommitsResult() (bool, error) { return f.unpushed, nil }
func (f fakeStrayProbe) HasStashesResult() (bool, error)         { return f.stashes, nil }
func (f fakeStrayProbe) WorktreeRemove(string, bool) error       { return nil }

// mkCheckout creates dir/.git (a directory, mimicking an unregistered clone)
// so the scan treats dir as a git checkout.
func mkCheckout(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestScanStrayWorktrees is the report-only guard for gcw-noyd slice 1: it must
// surface git checkouts under a managed root that are NOT bound to a live
// session, classify each reclaimable only when clean (no uncommitted / unpushed
// / stashed work), and never descend into or flag a live-bound worktree.
func TestScanStrayWorktrees(t *testing.T) {
	root := t.TempDir()
	live := mkCheckout(t, filepath.Join(root, "live-crew"))
	orphanClean := mkCheckout(t, filepath.Join(root, "orphan-clean"))
	orphanDirty := mkCheckout(t, filepath.Join(root, "orphan-dirty"))
	orphanUnpushed := mkCheckout(t, filepath.Join(root, "orphan-unpushed"))
	// A plain directory with no .git must be ignored entirely.
	if err := os.MkdirAll(filepath.Join(root, "plain-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	liveSet := map[string]bool{filepath.Clean(live): true}
	probes := map[string]gitProbe{
		orphanClean:    fakeStrayProbe{isRepo: true},
		orphanDirty:    fakeStrayProbe{isRepo: true, uncommitted: true},
		orphanUnpushed: fakeStrayProbe{isRepo: true, unpushed: true},
	}
	probeFor := func(dir string) gitProbe {
		if p, ok := probes[filepath.Clean(dir)]; ok {
			return p
		}
		t.Fatalf("unexpected probe for %q (a live-bound or non-checkout dir was classified)", dir)
		return nil
	}

	got, err := scanStrayWorktrees([]string{root}, liveSet, probeFor)
	if err != nil {
		t.Fatalf("scanStrayWorktrees: %v", err)
	}

	byPath := map[string]strayWorktree{}
	for _, s := range got {
		byPath[s.Path] = s
	}

	// live-crew and plain-dir must never appear.
	if _, ok := byPath[filepath.Clean(live)]; ok {
		t.Errorf("live-bound worktree must not be reported as stray")
	}
	if _, ok := byPath[filepath.Clean(filepath.Join(root, "plain-dir"))]; ok {
		t.Errorf("non-checkout dir must not be reported")
	}

	assertStray(t, byPath, orphanClean, true, "")
	assertStray(t, byPath, orphanDirty, false, "uncommitted changes")
	assertStray(t, byPath, orphanUnpushed, false, "unpushed commits")

	// Exactly the three orphans, nothing else.
	if len(got) != 3 {
		paths := make([]string, 0, len(got))
		for _, s := range got {
			paths = append(paths, s.Path)
		}
		sort.Strings(paths)
		t.Fatalf("expected 3 stray checkouts, got %d: %v", len(got), paths)
	}
}

func assertStray(t *testing.T, byPath map[string]strayWorktree, dir string, wantReclaimable bool, wantReason string) {
	t.Helper()
	s, ok := byPath[filepath.Clean(dir)]
	if !ok {
		t.Fatalf("expected stray entry for %q, missing", dir)
	}
	if s.Reclaimable != wantReclaimable {
		t.Errorf("%q reclaimable = %v, want %v (reason=%q)", dir, s.Reclaimable, wantReclaimable, s.Reason)
	}
	if wantReason != "" && s.Reason != wantReason {
		t.Errorf("%q reason = %q, want %q", dir, s.Reason, wantReason)
	}
}

// TestScanStrayWorktrees_DoesNotDescendIntoCheckout confirms slice-1 scope: a
// checkout found under the root is classified but not recursed into, so a repo
// nested inside another checkout is not double-reported at this stage.
func TestScanStrayWorktrees_DoesNotDescendIntoCheckout(t *testing.T) {
	root := t.TempDir()
	outer := mkCheckout(t, filepath.Join(root, "outer"))
	nested := mkCheckout(t, filepath.Join(outer, "nested"))

	probeFor := func(dir string) gitProbe {
		if filepath.Clean(dir) == filepath.Clean(nested) {
			t.Fatalf("scan descended into a checkout and classified nested %q", nested)
		}
		return fakeStrayProbe{isRepo: true}
	}

	got, err := scanStrayWorktrees([]string{root}, map[string]bool{}, probeFor)
	if err != nil {
		t.Fatalf("scanStrayWorktrees: %v", err)
	}
	if len(got) != 1 || got[0].Path != filepath.Clean(outer) {
		t.Fatalf("expected only the outer checkout, got %#v", got)
	}
}
