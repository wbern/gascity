//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestArchiveAndOrdinaryReaperProtectHiddenIndexFlags(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		for _, modified := range []bool{false, true} {
			name := strings.TrimPrefix(flag, "--") + "/clean"
			if modified {
				name = strings.TrimPrefix(flag, "--") + "/modified"
			}
			t.Run(name, func(t *testing.T) {
				city, rig := initReapRig(t)
				const id = "ga-hidden01"
				wt := addClosedWorktree(t, rig, city, "tasks", id)
				injectLiveness(t, liveWorktreeState{scanned: true})
				previous := archiveHandleSnapshot
				archiveHandleSnapshot = func(_ context.Context) ([]byte, error) {
					return []byte(archiveTestHandleRecord(t, os.Getpid(), "cwd", "r", "DIR", rig, rig)), nil
				}
				t.Cleanup(func() { archiveHandleSnapshot = previous })
				head := productionLifecycleGitOutput(t, wt, "rev-parse", "HEAD")
				branch := productionLifecycleGitOutput(t, wt, "symbolic-ref", "--short", "HEAD")
				store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: id, Status: "open", Metadata: map[string]string{"gc.task_worktree": wt, "gc.work_branch": branch, "gc.work_dir": wt}}}, nil)
				if err := store.Close(id); err != nil {
					t.Fatal(err)
				}
				closed, err := store.Get(id)
				if err != nil {
					t.Fatal(err)
				}
				scope := reapTestScope(map[string]beads.Store{reapTestRigName: store})
				cfg := reapTestConfig(rig)
				evidence := []byte(`{"fixture":"completed task with hidden-index regression"}`)
				evidencePath := filepath.Join(t.TempDir(), "evidence.json")
				if err := os.WriteFile(evidencePath, evidence, 0o600); err != nil {
					t.Fatal(err)
				}
				pointer, err := os.Lstat(filepath.Join(wt, ".git"))
				if err != nil {
					t.Fatal(err)
				}
				device, inode, ok := fsys.FileIdentity(pointer)
				if !ok {
					t.Fatal("fixture lacks inode identity")
				}
				req := worktreeArchiveRequest{SchemaVersion: "1", Rig: reapTestRigName, BeadID: id, Path: wt, ExpectedHEAD: head, ExpectedBranch: "refs/heads/" + branch, ExpectedBeadRevision: closed.Revision, ExpectedGitDir: productionLifecycleGitOutput(t, wt, "rev-parse", "--absolute-git-dir"), ExpectedPointerDevice: device, ExpectedPointerInode: inode, ProductionRef: "refs/remotes/origin/main", ExpectedProductionHEAD: head, IntegratedCommit: head, EvidencePath: evidencePath, EvidenceSHA256: archiveSHA(evidence)}
				var warnings bytes.Buffer
				control := archiveReapWorktree(city, cfg, scope, nil, req, false, &warnings)
				if control.Outcome != "would_reap" || !control.Complete {
					t.Fatalf("ordinary clean fixture not eligible: %+v %s", control, warnings.String())
				}
				req.ExpectedInventorySHA256 = control.InventorySHA256
				mustGit(t, wt, "update-index", flag, "README.md")
				expected := []byte("seed\n")
				if modified {
					expected = []byte("UNCOMMITTED SOURCE MUST SURVIVE\n")
					if err := os.WriteFile(filepath.Join(wt, "README.md"), expected, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				flagsBefore := gitStdout(t, wt, "ls-files", "-v", "-z")
				if raw := gitStdout(t, wt, "status", "--porcelain=v1", "-z", "--ignored"); raw != "" {
					t.Fatalf("fixture did not mask index WIP: %q", raw)
				}
				for _, execute := range []bool{false, true} {
					result := archiveReapWorktree(city, cfg, scope, nil, req, execute, &warnings)
					if result.Outcome == "would_reap" || result.Outcome == "reaped" || !strings.Contains(strings.ToLower(strings.Join(result.Reasons, " ")), "index") {
						t.Fatalf("masked index state admitted or wrong guard (execute%v): %+v", execute, result)
					}
					if result.ArchivePath != "" {
						t.Fatalf("flagged checkout reached archival writes: %+v", result)
					}
				}
				ordinary := reapClosedBeadWorktrees(city, cfg, scope, nil, false, events.Discard, nil, &warnings)
				if len(ordinary.Reaped) != 0 {
					t.Fatalf("ordinary reaper removed hidden-index source: %+v", ordinary)
				}
				actual, err := os.ReadFile(filepath.Join(wt, "README.md"))
				if err != nil || string(actual) != string(expected) {
					t.Fatalf("hidden-index source lost: %q %v", actual, err)
				}
				if after := gitStdout(t, wt, "ls-files", "-v", "-z"); after != flagsBefore {
					t.Fatalf("inspection mutated index flags: before%q after%q", flagsBefore, after)
				}
			})
		}
	}
}
