package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pathutil"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestRetiredWorkerDirRetriesWhenWorkBecomesSafe(t *testing.T) {
	for _, resolution := range []string{"restore", "commit"} {
		t.Run(resolution, func(t *testing.T) {
			city, rig := initReapRig(t)
			previousHandles := archiveHandleSnapshot
			archiveHandleSnapshot = func(_ context.Context) ([]byte, error) {
				return []byte(archiveTestHandleRecord(t, os.Getpid(), "cwd", "r", "DIR", rig, rig)), nil
			}
			t.Cleanup(func() { archiveHandleSnapshot = previousHandles })
			wt := addClosedWorktree(t, rig, city, "polecats", "retired-worker")
			on := true
			zero := 0
			cfg := &config.City{Workspace: config.Workspace{Prefix: "ga"}, Rigs: []config.Rig{{Name: reapTestRigName, Path: rig}}, Daemon: config.DaemonConfig{AutoPruneWorkerDir: &on, AutoReapClosedBeadWorktreesMaxLoadPercentValue: &zero, AutoReapClosedBeadWorktreesPaceMillis: &zero}}
			sessionID := "session-retired"
			sessions := beads.NewMemStoreFrom(1, []beads.Bead{{ID: sessionID, Status: "closed", Metadata: map[string]string{contract.WorkerDirKey: wt, "template": reapTestRigName + "/transient"}}}, nil)
			scope := reapTestScope(map[string]beads.Store{reapTestRigName: beads.NewMemStore()})
			injectLiveness(t, liveWorktreeState{scanned: true})
			source := filepath.Join(wt, "README.md")
			if err := os.WriteFile(source, []byte("unfinished user work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var diagnostics bytes.Buffer
			pruneAgentHomeWorktreeIfSafeInfo(sessionpkg.Info{ID: sessionID, WorkerDir: wt, Template: reapTestRigName + "/transient"}, city, cfg, &diagnostics)
			data, err := os.ReadFile(source)
			if err != nil || string(data) != "unfinished user work\n" {
				t.Fatalf("retirement lost work: %q %v", data, err)
			}
			assertNoWorktreeStaleMarker(t, wt)
			if got := retryDeferredWorkerDirPrunes(city, cfg, scope, sessions, nil, &diagnostics); got != 0 {
				t.Fatal("dirty retirement was retried destructively")
			}
			// The user resolves the dirt after its session is already closed. No new
			// retirement event occurs: only the real periodic retry entrypoint runs.
			if resolution == "commit" {
				mustGit(t, wt, "add", "README.md")
				mustGit(t, wt, "-c", "commit.gpgsign=false", "commit", "-m", "preserve completed work")
			} else {
				mustGit(t, wt, "restore", "README.md")
			}
			expectedHead := productionLifecycleGitOutput(t, wt, "rev-parse", "HEAD")
			if got := retryDeferredWorkerDirPrunes(city, cfg, scope, sessions, nil, &diagnostics); got != 1 {
				t.Fatalf("safe retired checkout never retried: removed=%d; %s", got, diagnostics.String())
			}
			if _, err := os.Stat(wt); !os.IsNotExist(err) {
				t.Fatalf("retired checkout remains: %v", err)
			}
			if got := productionLifecycleGitOutput(t, rig, "rev-parse", "refs/heads/wt-retired-worker"); got != expectedHead {
				t.Fatal("source branch lost")
			}
		})
	}
}

func TestRetiredWorkerDirRetryPreservesProtectedWork(t *testing.T) {
	for _, scenario := range []string{"dirty", "ignored", "user_marker", "live_cwd", "live_session", "held_file", "unknown_descriptor", "late_borrower", "late_live_cwd", "late_reopened_owner", "late_unpreserved_commit", "borrower", "incomplete_scope", "reopened_owner", "reused_identity", "new_branch", "registry_symlink", "corrupt_record", "cursor_content", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			city, rig := initReapRig(t)
			previousHandles := archiveHandleSnapshot
			archiveHandleSnapshot = func(_ context.Context) ([]byte, error) {
				return []byte(archiveTestHandleRecord(t, os.Getpid(), "cwd", "r", "DIR", rig, rig)), nil
			}
			t.Cleanup(func() { archiveHandleSnapshot = previousHandles })
			wt := addClosedWorktree(t, rig, city, "polecats", "retired-guard")
			if scenario == "late_unpreserved_commit" {
				mustGit(t, wt, "checkout", "--detach")
			}
			on, zero := true, 0
			cfg := &config.City{Workspace: config.Workspace{Prefix: "ga"}, Rigs: []config.Rig{{Name: reapTestRigName, Path: rig}}, Daemon: config.DaemonConfig{AutoPruneWorkerDir: &on, AutoReapClosedBeadWorktreesMaxLoadPercentValue: &zero, AutoReapClosedBeadWorktreesPaceMillis: &zero}}
			id := "session-retired"
			sessions := beads.NewMemStoreFrom(1, []beads.Bead{{ID: id, Status: "closed", Metadata: map[string]string{contract.WorkerDirKey: wt, "template": reapTestRigName + "/transient"}}}, nil)
			scope := reapTestScope(map[string]beads.Store{reapTestRigName: beads.NewMemStore()})
			injectLiveness(t, liveWorktreeState{scanned: true})
			if scenario == "user_marker" {
				writeFileAt(t, wt, worktreeStaleFileName, "my private note; not a GC receipt\n")
			}
			writeFileAt(t, wt, "README.md", "dirty before retirement\n")
			var diagnostics bytes.Buffer
			pruneAgentHomeWorktreeIfSafeInfo(sessionpkg.Info{ID: id, WorkerDir: wt, Template: reapTestRigName + "/transient"}, city, cfg, &diagnostics)
			queue := filepath.Join(city, ".gc", "runtime", workerDirRetryDirectory)
			records, err := filepath.Glob(filepath.Join(queue, "*.json"))
			if err != nil || len(records) != 1 {
				t.Fatalf("missing durable retirement record: %v %v %s", records, err, diagnostics.String())
			}
			if scenario != "dirty" {
				mustGit(t, wt, "restore", "README.md")
			}
			var liveDirs []string
			outsideRegistry := ""
			switch scenario {
			case "ignored":
				writeFileAt(t, rig, ".git/info/exclude", ".env.local\n")
				writeFileAt(t, wt, ".env.local", "preserve custom configuration\n")
			case "live_cwd":
				state := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(wt)}}
				if yes, _ := worktreeIsLive(wt, state, nil); !yes {
					t.Fatal("fixture does not satisfy the real collector's path contract")
				}
				injectLiveness(t, state)
			case "live_session":
				liveDirs = []string{wt}
			case "held_file":
				file, err := os.Open(filepath.Join(wt, "README.md"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := file.Close(); err != nil {
						t.Error(err)
					}
				})
				archiveHandleSnapshot = func(_ context.Context) ([]byte, error) {
					return []byte(archiveTestHandleRecord(t, os.Getpid(), "cwd", "r", "DIR", rig, rig) + archiveTestHandleRecord(t, os.Getpid(), fmt.Sprint(file.Fd()), "r", "REG", filepath.Join(wt, "README.md"), filepath.Join(wt, "README.md"))), nil
				}
			case "unknown_descriptor":
				archiveHandleSnapshot = func(_ context.Context) ([]byte, error) {
					return []byte(fmt.Sprintf("p%d\x00f10\x00n(revoked)\x00", os.Getpid())), nil
				}
			case "late_borrower":
				archiveHandleSnapshot = func(_ context.Context) ([]byte, error) {
					if _, err := scope.CityStore.Create(beads.Bead{Status: "open", Metadata: map[string]string{"gc.work_dir": wt}}); err != nil {
						t.Fatal(err)
					}
					return []byte(archiveTestHandleRecord(t, os.Getpid(), "cwd", "r", "DIR", rig, rig)), nil
				}
			case "late_live_cwd":
				archiveHandleSnapshot = func(_ context.Context) ([]byte, error) {
					state := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(wt)}}
					if yes, _ := worktreeIsLive(wt, state, nil); !yes {
						t.Fatal("late cwd fixture does not satisfy the real collector's path contract")
					}
					collectLiveWorktreeStateFn = func() liveWorktreeState { return state }
					return []byte(archiveTestHandleRecord(t, os.Getpid(), "cwd", "r", "DIR", rig, rig)), nil
				}
			case "late_reopened_owner":
				archiveHandleSnapshot = func(_ context.Context) ([]byte, error) {
					if err := sessions.Reopen(id); err != nil {
						t.Fatal(err)
					}
					return []byte(archiveTestHandleRecord(t, os.Getpid(), "cwd", "r", "DIR", rig, rig)), nil
				}
			case "late_unpreserved_commit":
				archiveHandleSnapshot = func(_ context.Context) ([]byte, error) {
					writeFileAt(t, wt, "README.md", "new detached work during inspection\n")
					mustGit(t, wt, "add", "README.md")
					mustGit(t, wt, "-c", "commit.gpgsign=false", "commit", "-m", "preserve this new work")
					return []byte(archiveTestHandleRecord(t, os.Getpid(), "cwd", "r", "DIR", rig, rig)), nil
				}
			case "borrower":
				scope.CityStore = beads.NewMemStoreFrom(1, []beads.Bead{{ID: "borrower", Status: "open", Metadata: map[string]string{"gc.work_dir": filepath.Join(wt, "subproject")}}}, nil)
			case "incomplete_scope":
				scope.CityStore = nil
			case "reopened_owner":
				if err := sessions.Reopen(id); err != nil {
					t.Fatal(err)
				}
			case "reused_identity":
				mustGit(t, rig, "worktree", "remove", wt)
				mustGit(t, rig, "worktree", "add", wt, "wt-retired-guard")
			case "new_branch":
				mustGit(t, wt, "checkout", "-b", "new-user-owner")
			case "registry_symlink":
				outside := t.TempDir()
				outsideRegistry = outside
				writeFileAt(t, outside, "operator.txt", "untouched\n")
				if err := os.Rename(queue, queue+"-retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, queue); err != nil {
					t.Fatal(err)
				}
			case "cursor_content":
				writeFileAt(t, queue, ".cursor", "operator text must survive\n")
				if err := os.Chmod(filepath.Join(queue, ".cursor"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "corrupt_record":
				if err := os.WriteFile(records[0], []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			removed := 0
			if scenario == "canceled" {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				removed = retryDeferredWorkerDirPrunesCtx(ctx, city, cfg, scope, sessions, liveDirs, &diagnostics)
			} else {
				removed = retryDeferredWorkerDirPrunes(city, cfg, scope, sessions, liveDirs, &diagnostics)
			}
			if removed != 0 {
				t.Fatalf("protected retirement removed: %d %s", removed, diagnostics.String())
			}
			if _, err := os.Stat(wt); err != nil {
				t.Fatalf("protected checkout lost: %v", err)
			}
			if outsideRegistry != "" {
				entries, err := os.ReadDir(outsideRegistry)
				if err != nil || len(entries) != 1 || entries[0].Name() != "operator.txt" {
					t.Fatal("aliased registry was modified")
				}
			}
			if scenario == "cursor_content" {
				data, err := os.ReadFile(filepath.Join(queue, ".cursor"))
				if err != nil || string(data) != "operator text must survive\n" {
					t.Fatal("unknown cursor content overwritten")
				}
			}
			if scenario == "user_marker" {
				data, err := os.ReadFile(filepath.Join(wt, worktreeStaleFileName))
				if err != nil || string(data) != "my private note; not a GC receipt\n" {
					t.Fatalf("user marker changed: %q %v", data, err)
				}
			}
			if scenario == "dirty" {
				data, err := os.ReadFile(filepath.Join(wt, "README.md"))
				if err != nil || string(data) != "dirty before retirement\n" {
					t.Fatalf("dirty source changed: %q %v", data, err)
				}
			}
			if scenario == "ignored" {
				data, err := os.ReadFile(filepath.Join(wt, ".env.local"))
				if err != nil || string(data) != "preserve custom configuration\n" {
					t.Fatalf("ignored config changed: %q %v", data, err)
				}
			}
		})
	}
}

func TestRetiredWorkerDirRetryPreservesUnreferencedCommitUntilPreserved(t *testing.T) {
	city, rig := initReapRig(t)
	previousHandles := archiveHandleSnapshot
	archiveHandleSnapshot = func(_ context.Context) ([]byte, error) {
		return []byte(archiveTestHandleRecord(t, os.Getpid(), "cwd", "r", "DIR", rig, rig)), nil
	}
	t.Cleanup(func() { archiveHandleSnapshot = previousHandles })
	wt := addClosedWorktree(t, rig, city, "polecats", "retired-detached")
	mustGit(t, wt, "checkout", "--detach")
	writeFileAt(t, wt, "README.md", "unreferenced user work\n")
	mustGit(t, wt, "add", "README.md")
	mustGit(t, wt, "-c", "commit.gpgsign=false", "commit", "-m", "unreferenced work")
	head := productionLifecycleGitOutput(t, wt, "rev-parse", "HEAD")
	on, zero := true, 0
	cfg := &config.City{Workspace: config.Workspace{Prefix: "ga"}, Rigs: []config.Rig{{Name: reapTestRigName, Path: rig}}, Daemon: config.DaemonConfig{AutoPruneWorkerDir: &on, AutoReapClosedBeadWorktreesMaxLoadPercentValue: &zero, AutoReapClosedBeadWorktreesPaceMillis: &zero}}
	id := "session-retired"
	sessions := beads.NewMemStoreFrom(1, []beads.Bead{{ID: id, Status: "closed", Metadata: map[string]string{contract.WorkerDirKey: wt, "template": reapTestRigName + "/transient"}}}, nil)
	scope := reapTestScope(map[string]beads.Store{reapTestRigName: beads.NewMemStore()})
	injectLiveness(t, liveWorktreeState{scanned: true})
	var diagnostics bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(sessionpkg.Info{ID: id, WorkerDir: wt, Template: reapTestRigName + "/transient"}, city, cfg, &diagnostics)
	if got := retryDeferredWorkerDirPrunes(city, cfg, scope, sessions, nil, &diagnostics); got != 0 {
		t.Fatal("unreferenced commit lost")
	}
	if got := productionLifecycleGitOutput(t, wt, "rev-parse", "HEAD"); got != head {
		t.Fatal("detached work changed")
	}
	mustGit(t, rig, "update-ref", "refs/heads/preserved-retired", head)
	if got := retryDeferredWorkerDirPrunes(city, cfg, scope, sessions, nil, &diagnostics); got != 1 {
		t.Fatalf("preserved detached retirement not retried: %s", diagnostics.String())
	}
	if got := productionLifecycleGitOutput(t, rig, "rev-parse", "refs/heads/preserved-retired"); got != head {
		t.Fatal("preservation ref lost")
	}
}

func TestRetiredWorkerDirRetryCadence(t *testing.T) {
	enabled := true
	cr := &CityRuntime{cfg: &config.City{Daemon: config.DaemonConfig{AutoPruneWorkerDir: &enabled}}}
	now := time.Unix(100, 0)
	if !cr.workerDirRetryDue(now) {
		t.Fatal("first periodic pass omitted")
	}
	for i := 0; i < 60; i++ {
		if cr.workerDirRetryDue(now.Add(time.Duration(i) * time.Second)) {
			t.Fatal("ordinary ticks repeated expensive retirement scan")
		}
	}
	if !cr.workerDirRetryDue(now.Add(time.Minute)) {
		t.Fatal("periodic retry never becomes due")
	}
	enabled = false
	if cr.workerDirRetryDue(now.Add(2 * time.Minute)) {
		t.Fatal("disabled pruning admitted")
	}
}

func TestRetiredWorkerDirRetryBatchRotation(t *testing.T) {
	city, err := normalizeReapBorrowPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := openWorkerDirRetryRoot(city, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	for i := 0; i < workerDirRetryBatch+3; i++ {
		data, err := json.Marshal(workerDirRetirement{Version: 1, SessionID: fmt.Sprintf("session-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if err := writeWorkerDirRetryFile(root, hex.EncodeToString(sum[:])+".json", data, false); err != nil {
			t.Fatal(err)
		}
	}
	first, err := pendingWorkerDirs(root)
	if err != nil || len(first) != workerDirRetryBatch {
		t.Fatalf("unbounded or missing batch: %d %v", len(first), err)
	}
	if _, err := root.Lstat(".cursor"); !os.IsNotExist(err) {
		t.Fatal("merely selecting a batch advanced the cursor")
	}
	// A pass that only attempted three entries must not skip its unattempted tail.
	if err := writeWorkerDirRetryFile(root, ".cursor", []byte(first[2].name), true); err != nil {
		t.Fatal(err)
	}
	next, err := pendingWorkerDirs(root)
	if err != nil || len(next) != workerDirRetryBatch || next[0].name != first[3].name {
		t.Fatalf("partial pass starved its tail: %v", err)
	}
	if err := writeWorkerDirRetryFile(root, ".cursor", []byte(next[len(next)-1].name), true); err != nil {
		t.Fatal(err)
	}
	wrapped, err := pendingWorkerDirs(root)
	if err != nil || len(wrapped) == 0 {
		t.Fatalf("rotation stopped: %v", err)
	}
	if wrapped[0].name != first[0].name {
		t.Fatal("cursor failed to wrap for another retry")
	}
}

func TestRetiredWorkerDirIdentityRejectsRedirectedWorktree(t *testing.T) {
	city, rig := initReapRig(t)
	wt := addClosedWorktree(t, rig, city, "polecats", "retired-redirect")
	if _, err := workerDirRetirementIdentity(city, reapTestRigName, rig, wt, "session-retired"); err != nil {
		t.Fatalf("ordinary worktree identity refused: %v", err)
	}
	outside, err := normalizeReapBorrowPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, outside, "README.md", "seed\n")
	writeFileAt(t, wt, "README.md", "USER WIP\n")
	mustGit(t, rig, "config", "extensions.worktreeConfig", "true")
	mustGit(t, wt, "config", "--worktree", "core.worktree", outside)
	if got := productionLifecycleGitOutput(t, wt, "rev-parse", "--show-toplevel"); got != outside {
		t.Fatalf("fixture did not redirect Git inspection: %q", got)
	}
	if got := productionLifecycleGitOutput(t, wt, "status", "--porcelain"); got != "" {
		t.Fatalf("redirected status must reproduce false cleanliness: %q", got)
	}
	if _, err := workerDirRetirementIdentity(city, reapTestRigName, rig, wt, "session-retired"); err == nil {
		t.Fatal("retirement accepted an identity whose Git probes inspect another root")
	}
	data, err := os.ReadFile(filepath.Join(wt, "README.md"))
	if err != nil || string(data) != "USER WIP\n" {
		t.Fatalf("candidate source changed: %q %v", data, err)
	}
}

func TestRetiredWorkerDirRetryRecoversCompletedPublicationAfterCrash(t *testing.T) {
	for _, scenario := range []string{"published_pending", "partial_cleanup", "external_only", "mixed_external", "unknown_alias", "symlink_alias", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			city, rig := initReapRig(t)
			wt := addClosedWorktree(t, rig, city, "polecats", "retired-crash")
			on, zero := true, 0
			cfg := &config.City{Workspace: config.Workspace{Prefix: "ga"}, Rigs: []config.Rig{{Name: reapTestRigName, Path: rig}}, Daemon: config.DaemonConfig{AutoPruneWorkerDir: &on, AutoReapClosedBeadWorktreesMaxLoadPercentValue: &zero, AutoReapClosedBeadWorktreesPaceMillis: &zero}}
			id := "session-retired"
			sessions := beads.NewMemStoreFrom(1, []beads.Bead{{ID: id, Status: "closed", Metadata: map[string]string{contract.WorkerDirKey: wt, "template": reapTestRigName + "/transient"}}}, nil)
			scope := reapTestScope(map[string]beads.Store{reapTestRigName: beads.NewMemStore()})
			injectLiveness(t, liveWorktreeState{scanned: true})
			previousHandles := archiveHandleSnapshot
			archiveHandleSnapshot = func(_ context.Context) ([]byte, error) {
				return []byte(archiveTestHandleRecord(t, os.Getpid(), "cwd", "r", "DIR", rig, rig)), nil
			}
			t.Cleanup(func() { archiveHandleSnapshot = previousHandles })
			writeFileAt(t, wt, "README.md", "unfinished before retirement\n")
			var diagnostics bytes.Buffer
			pruneAgentHomeWorktreeIfSafeInfo(sessionpkg.Info{ID: id, WorkerDir: wt, Template: reapTestRigName + "/transient"}, city, cfg, &diagnostics)
			queue := filepath.Join(city, ".gc", "runtime", workerDirRetryDirectory)
			records, err := filepath.Glob(filepath.Join(queue, "*.json"))
			if err != nil || len(records) != 1 {
				t.Fatalf("missing published retirement: %v %v %s", records, err, diagnostics.String())
			}
			final := records[0]
			pending := filepath.Join(queue, ".pending-"+strings.Repeat("A", 26))
			paths := []string{final}
			if scenario != "external_only" {
				if err := os.Link(final, pending); err != nil {
					t.Fatal(err)
				}
				paths = append(paths, pending)
			}
			switch scenario {
			case "partial_cleanup":
				other := filepath.Join(queue, ".pending-"+strings.Repeat("B", 26))
				if err := os.Link(final, other); err != nil {
					t.Fatal(err)
				}
				// A first recovery removed one alias, then the controller died.
				if err := os.Remove(other); err != nil {
					t.Fatal(err)
				}
			case "external_only", "mixed_external":
				outside := filepath.Join(t.TempDir(), "operator-owned-alias")
				if err := os.Link(final, outside); err != nil {
					t.Fatal(err)
				}
				paths = append(paths, outside)
			case "unknown_alias":
				unknown := filepath.Join(queue, ".pending-user-note")
				if err := os.Link(final, unknown); err != nil {
					t.Fatal(err)
				}
				paths = append(paths, unknown)
			case "symlink_alias":
				outside := filepath.Join(t.TempDir(), "operator.txt")
				if err := os.WriteFile(outside, []byte("user metadata must survive\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(queue, ".pending-"+strings.Repeat("C", 26))
				if err := os.Symlink(outside, link); err != nil {
					t.Fatal(err)
				}
				paths = append(paths, link, outside)
			case "corrupt":
				if err := os.WriteFile(final, []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := make(map[string]os.FileInfo)
			contents := make(map[string][]byte)
			for _, path := range paths {
				before[path], err = os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				contents[path], err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			mustGit(t, wt, "restore", "README.md")
			// Re-enter only the durable periodic consumer, as after restart.
			removed := retryDeferredWorkerDirPrunes(city, cfg, scope, sessions, nil, &diagnostics)
			if scenario == "published_pending" || scenario == "partial_cleanup" {
				if removed != 1 {
					t.Fatalf("complete publication stranded by its orphan pending alias: removed=%d %s", removed, diagnostics.String())
				}
				for _, path := range append(paths, wt) {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatalf("completed retirement metadata or checkout remains: %s %v", path, err)
					}
				}
				if got := retryDeferredWorkerDirPrunes(city, cfg, scope, sessions, nil, &diagnostics); got != 0 {
					t.Fatal("completed retry was not idempotent")
				}
				return
			}
			if removed != 0 {
				t.Fatalf("uncertain publication authorized retirement: %s", scenario)
			}
			if _, err := os.Stat(wt); err != nil {
				t.Fatal("protected checkout lost")
			}
			for _, path := range paths {
				after, err := os.Lstat(path)
				data, readErr := os.ReadFile(path)
				if err != nil || readErr != nil || !os.SameFile(before[path], after) || !bytes.Equal(contents[path], data) {
					t.Fatalf("unapproved metadata alias changed: %s %v %v", path, err, readErr)
				}
			}
		})
	}
}
