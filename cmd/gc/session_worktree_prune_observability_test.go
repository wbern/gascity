package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The worker_dir auto-prune path had five early returns that reported nothing:
// no worker_dir metadata, a relative worker_dir, a worker_dir outside the
// city's worktree tree, a missing .git pointer, and a non-repo. Each is a
// correct decision, and each was invisible.
//
// The cost is not hypothetical. Asked "why is the reconciler prune never
// logging", a whole session of archeology could not distinguish "never called"
// from "called and skipped" from the logs alone — the question had to be
// answered by reading the source and then cross-checking a second signal (zero
// pool-session closes in the window). These tests make each skip state its
// reason, so the next reader gets the answer from the log.
//
// Config-disabled is deliberately excluded: an operator who set
// auto_prune_worker_dir = false does not need to be told once per session
// close. TestPruneWorkerDirSkip_ConfigDisabledStaysSilent pins that intent.

// pruneSkipCase is one silent-skip scenario: it mutates the fixture into the
// state under test and names the substring the log line must carry.
type pruneSkipCase struct {
	name      string
	wantInLog string
	mutate    func(fx *pruneTestFixture) (workerDirOverride string)
}

func pruneSkipCases() []pruneSkipCase {
	return []pruneSkipCase{
		{
			name:      "no worker_dir metadata",
			wantInLog: "no worker_dir",
			mutate:    func(_ *pruneTestFixture) string { return "" },
		},
		{
			name:      "relative worker_dir",
			wantInLog: "absolute",
			mutate:    func(_ *pruneTestFixture) string { return filepath.Join("relative", "worker") },
		},
		{
			name:      "worker_dir outside the city worktree tree",
			wantInLog: "worktrees",
			mutate: func(fx *pruneTestFixture) string {
				outside := filepath.Join(fx.t.TempDir(), "elsewhere")
				if err := os.MkdirAll(outside, 0o755); err != nil {
					fx.t.Fatalf("mkdir outside dir: %v", err)
				}
				return outside
			},
		},
		{
			name:      "missing .git pointer",
			wantInLog: ".git",
			mutate: func(fx *pruneTestFixture) string {
				if err := os.Remove(filepath.Join(fx.workerDir, ".git")); err != nil {
					fx.t.Fatalf("remove .git marker: %v", err)
				}
				return fx.workerDir
			},
		},
		{
			name:      "worker_dir is not a git repo",
			wantInLog: "repo",
			mutate: func(fx *pruneTestFixture) string {
				fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: false})
				return fx.workerDir
			},
		},
	}
}

// TestPruneWorkerDirSkip_BeadFormReportsEverySilentSkip covers the raw
// beads.Bead entry point.
func TestPruneWorkerDirSkip_BeadFormReportsEverySilentSkip(t *testing.T) {
	for _, tc := range pruneSkipCases() {
		t.Run(tc.name, func(t *testing.T) {
			fx := newPruneFixture(t)
			workerDir := tc.mutate(fx)
			session := fx.sessionBead()
			if workerDir == "" {
				delete(session.Metadata, "worker_dir")
			} else {
				session.Metadata["worker_dir"] = workerDir
			}

			var stderr bytes.Buffer
			if pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, &stderr) {
				t.Fatal("prune returned true for a skip case")
			}
			if rigProbe := fx.probesByWD[fx.rigRoot]; rigProbe != nil && rigProbe.removeInvoked {
				t.Fatal("WorktreeRemove called for a skip case")
			}
			assertPruneSkipLogged(t, stderr.String(), tc.wantInLog)
		})
	}
}

// TestPruneWorkerDirSkip_InfoFormReportsEverySilentSkip covers the session.Info
// entry point, which the live reconciler actually calls. Both forms must
// explain themselves identically; a reason that only the untaken path reports
// is no help to an operator.
func TestPruneWorkerDirSkip_InfoFormReportsEverySilentSkip(t *testing.T) {
	for _, tc := range pruneSkipCases() {
		t.Run(tc.name, func(t *testing.T) {
			fx := newPruneFixture(t)
			workerDir := tc.mutate(fx)
			info := fx.sessionInfo()
			info.WorkerDir = workerDir

			var stderr bytes.Buffer
			pruneAgentHomeWorktreeIfSafeInfo(info, fx.cityPath, fx.cfg, &stderr)
			if rigProbe := fx.probesByWD[fx.rigRoot]; rigProbe != nil && rigProbe.removeInvoked {
				t.Fatal("WorktreeRemove called for a skip case")
			}
			assertPruneSkipLogged(t, stderr.String(), tc.wantInLog)
		})
	}
}

// TestPruneWorkerDirSkip_ConfigDisabledStaysSilent is the noise guard: the one
// skip that is an operator's own standing choice must not be announced on every
// session close.
func TestPruneWorkerDirSkip_ConfigDisabledStaysSilent(t *testing.T) {
	fx := newPruneFixture(t)
	off := false
	fx.cfg.Daemon.AutoPruneWorkerDir = &off

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true while disabled")
	}
	pruneAgentHomeWorktreeIfSafeInfo(fx.sessionInfo(), fx.cityPath, fx.cfg, &stderr)
	if got := stderr.String(); got != "" {
		t.Errorf("stderr = %q, want silence when auto_prune_worker_dir is off", got)
	}
}

// TestPruneWorkerDirSkip_HappyPathLogsNoSkip guards the other direction: a
// successful prune must not emit a skip reason alongside its success line.
func TestPruneWorkerDirSkip_HappyPathLogsNoSkip(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true})
	rigProbe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.rigRoot, rigProbe)

	var stderr bytes.Buffer
	if !pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatalf("prune returned false on the happy path; stderr:\n%s", stderr.String())
	}
	if !rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove not called on the happy path")
	}
	if got := stderr.String(); strings.Contains(got, "not pruning") {
		t.Errorf("stderr = %q, want no skip reason on a successful prune", got)
	}
}

// assertPruneSkipLogged fails unless log carries a "not pruning" line naming
// the expected cause.
func assertPruneSkipLogged(t *testing.T, log, wantSubstring string) {
	t.Helper()
	if !strings.Contains(log, "not pruning") {
		t.Fatalf("stderr = %q, want a 'not pruning worker_dir' line explaining the skip", log)
	}
	if !strings.Contains(log, wantSubstring) {
		t.Errorf("stderr = %q, want it to mention %q", log, wantSubstring)
	}
}
