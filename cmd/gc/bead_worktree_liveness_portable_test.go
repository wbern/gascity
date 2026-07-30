package main

import (
	"errors"
	"runtime"
	"testing"
)

// The reaper's liveness scan read /proc and nothing else, so on any host without
// /proc — every macOS machine, this whole fleet — it returned scanned=false and
// the documented fail-closed rule protected 100% of candidates forever. Measured
// live before this change: `gc worktree reap` reported 0 would reap, 249 kept,
// every one of them "liveness scan unavailable (failing closed, protecting all)".
//
// These tests cover the portable fallback. The rule they encode: a scan that saw
// at least one process cwd is a scan, and a scan that saw nothing at all is not
// trusted — on a running host, zero process cwds means the enumeration failed,
// not that nothing is running.

func TestCollectLiveWorktreeStateFallback_ParsesLsofFieldOutput(t *testing.T) {
	stubLsof(t, "p433\nfcwd\nn/Users/w/city/.gc/worktrees/rig/a\np540\nfcwd\nn/Users/w/city/.gc/worktrees/rig/b\n", nil)

	got := collectLiveWorktreeStateFallback()

	if !got.scanned {
		t.Fatal("scanned = false, want true: the enumeration succeeded")
	}
	if len(got.cwds) != 2 {
		t.Fatalf("cwds = %v, want 2 entries", got.cwds)
	}
}

func TestCollectLiveWorktreeStateFallback_DeduplicatesSharedCwds(t *testing.T) {
	// Several processes sitting in one worktree is the normal case — an agent
	// plus whatever it spawned — and must count once.
	stubLsof(t, "p1\nfcwd\nn/w/tree\np2\nfcwd\nn/w/tree\np3\nfcwd\nn/w/tree\n", nil)

	got := collectLiveWorktreeStateFallback()

	if len(got.cwds) != 1 {
		t.Fatalf("cwds = %v, want 1 after dedup", got.cwds)
	}
}

func TestCollectLiveWorktreeStateFallback_SkipsNonPathRecords(t *testing.T) {
	// pid and fd records, blank lines, and relative paths carry no cwd.
	stubLsof(t, "p1\nfcwd\nn/abs/one\nfcwd\nnrelative/path\n\nn\np2\n", nil)

	got := collectLiveWorktreeStateFallback()

	if len(got.cwds) != 1 {
		t.Fatalf("cwds = %v, want only the absolute path", got.cwds)
	}
}

// TestCollectLiveWorktreeStateFallback_FailsClosedWhenCommandUnavailable is the
// safety case: no enumerator means no proof any tree is idle, which must protect
// everything rather than authorize deletion.
func TestCollectLiveWorktreeStateFallback_FailsClosedWhenCommandUnavailable(t *testing.T) {
	stubLsof(t, "", errors.New("exec: lsof: executable file not found in $PATH"))

	got := collectLiveWorktreeStateFallback()

	if got.scanned {
		t.Error("scanned = true with no enumerator available; the reaper would treat live trees as idle")
	}
}

// TestCollectLiveWorktreeStateFallback_FailsClosedOnEmptyOutput encodes the
// judgment that distinguishes this from a naive parse: a running host always has
// processes with a cwd, so an empty listing is a broken enumeration, not an idle
// machine.
func TestCollectLiveWorktreeStateFallback_FailsClosedOnEmptyOutput(t *testing.T) {
	stubLsof(t, "", nil)

	got := collectLiveWorktreeStateFallback()

	if got.scanned {
		t.Error("scanned = true on empty output; zero process cwds means the scan failed")
	}
}

// TestCollectLiveWorktreeStateFallback_PartialOutputStillCounts mirrors the Linux
// path deliberately. os.Readlink on /proc/<pid>/cwd fails with EACCES for other
// users' processes and upstream simply skips those, still reporting scanned=true.
// lsof behaves the same way — it cannot read other users' descriptors without
// privileges and warns on stderr while listing the rest — so a non-zero exit
// alongside real records is the same partial scan, not a failure.
func TestCollectLiveWorktreeStateFallback_PartialOutputStillCounts(t *testing.T) {
	stubLsof(t, "p1\nfcwd\nn/w/tree\n", errors.New("exit status 1"))

	got := collectLiveWorktreeStateFallback()

	if !got.scanned {
		t.Error("scanned = false for a partial listing; the Linux path treats unreadable processes the same way")
	}
	if len(got.cwds) != 1 {
		t.Errorf("cwds = %v, want the one readable record", got.cwds)
	}
}

// TestCollectLiveWorktreeState_ScansOnThisHost is the end-to-end proof, and the
// one that would have caught the original defect: on a real machine, with the
// real enumerator, the scan must come back usable. On darwin that exercises the
// fallback; on linux it exercises /proc.
func TestCollectLiveWorktreeState_ScansOnThisHost(t *testing.T) {
	got := collectLiveWorktreeStateFn()

	if !got.scanned {
		t.Fatalf("liveness scan unavailable on %s; the reaper protects every candidate forever in this state", runtime.GOOS)
	}
	if len(got.cwds) == 0 {
		t.Errorf("scan reported no process cwds on %s, which cannot be true of a running host", runtime.GOOS)
	}
}

// stubLsof replaces the process-cwd enumerator for one test.
func stubLsof(t *testing.T, out string, err error) {
	t.Helper()
	prev := liveWorktreeCwdEnumerator
	liveWorktreeCwdEnumerator = func() ([]byte, error) { return []byte(out), err }
	t.Cleanup(func() { liveWorktreeCwdEnumerator = prev })
}
