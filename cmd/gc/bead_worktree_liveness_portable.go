package main

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// liveWorktreeCwdTimeout bounds the enumeration. The reaper runs on the
// controller tick, so a hung process-table query must not stall it; a timeout
// yields no records, which fails closed and protects every candidate.
const liveWorktreeCwdTimeout = 20 * time.Second

// liveWorktreeCwdEnumerator lists every visible process's working directory in
// lsof field format (-F): one "p<pid>" record per process, "f<fd>" per
// descriptor, "n<path>" for the path. Indirected through a var so the parser and
// the fail-closed rules are testable without a process table.
var liveWorktreeCwdEnumerator = func() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), liveWorktreeCwdTimeout)
	defer cancel()
	// -a -d cwd restricts the listing to current-working-directory descriptors,
	// which is the only fd class the liveness gate cares about and keeps the
	// output small enough to parse on every tick.
	return exec.CommandContext(ctx, "lsof", "-a", "-d", "cwd", "-Fpn").Output()
}

// collectLiveWorktreeStateFallback enumerates process working directories on
// hosts without /proc, so the reaper's liveness gate has a real signal there
// instead of a permanent "indeterminate".
//
// Without this, collectLiveWorktreeState returned scanned=false on every macOS
// host, the gate failed closed, and the reaper protected 100% of candidates
// forever — measured live at 0 reaped / 249 kept, every one of them attributed
// to the unavailable scan. That is a whole platform on which the feature could
// not work, and it masked the gates it was being blamed for: liveness is
// evaluated before the git-state gates, so those never even ran.
//
// Two judgments are deliberate, and both are pinned by tests:
//
//   - No records at all means the scan FAILED, not that the host is idle. A
//     running machine always has processes with a working directory, so an empty
//     listing is reported as scanned=false and protects everything.
//   - Records alongside a non-zero exit is a PARTIAL scan, and counts. lsof
//     cannot read other users' descriptors unprivileged; it warns and lists the
//     rest. The /proc path has the identical blind spot — os.Readlink on another
//     user's /proc/<pid>/cwd fails with EACCES and that pid is skipped while the
//     scan still reports scanned=true — so treating a partial listing as a scan
//     keeps the two platforms honest with each other rather than holding one to a
//     standard the other never met.
func collectLiveWorktreeStateFallback() liveWorktreeState {
	out, err := liveWorktreeCwdEnumerator()

	seen := make(map[string]struct{})
	var cwds []string
	for _, line := range strings.Split(string(out), "\n") {
		// Only "n" records carry a path; "p"/"f" records identify the process
		// and descriptor, and lsof emits a trailing blank line.
		path, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "n")
		if !ok || !strings.HasPrefix(path, "/") {
			continue
		}
		canon := pathutil.NormalizePathForCompare(path)
		if canon == "" {
			continue
		}
		if _, dup := seen[canon]; dup {
			continue
		}
		seen[canon] = struct{}{}
		cwds = append(cwds, canon)
	}

	if len(cwds) == 0 {
		// Either the enumerator is missing/failed outright, or it returned
		// nothing usable. Both are indeterminate, and indeterminate protects.
		_ = err
		return liveWorktreeState{scanned: false}
	}
	return liveWorktreeState{cwds: cwds, scanned: true}
}
