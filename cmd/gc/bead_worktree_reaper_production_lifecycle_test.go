//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Actual provider materialization and a content-changing squash merge exercise
// the same native archive service as the CLI. The ordinary reaper must retain
// config; only exact completion plus verified archival may release the checkout.
func TestReapClosedBeadWorktrees_ProductionLifecycle(t *testing.T) {
	cases := []struct {
		name    string
		protect bool
	}{
		{"merged_codex_task", false},
		{"merged_claude_task", false},
		{"merged_runtime_staged_task", false},
		{"legacy_slug_task", false},
		{"custom_provider_config", false},
		{"preexisting_mcp_config", false},
		{"edited_mcp_config", false},
		{"handoff_note", false},
		{"extensionless_cli_version", false},
		{"tracked_edit_after_merge", true},
		{"untracked_user_file", true},
		{"unknown_ignored_source", true},
		{"nested_repository_in_cache", true},
		{"crew_namespace", true},
		{"nested_task_namespace", true},
		{"prefix_collision_namespace", true},
		{"open_borrower", true},
		{"city_borrower", true},
		{"held_descriptor", true},
		{"unknown_descriptor", true},
		{"transient_poisoned_index", true},
		{"user_paused", true},
		{"live_process", true},
		{"late_original_recreation", false},
		{"late_recreated_original_with_hold", false},
		{"late_unknown_quarantine_leaf", false},
		{"late_clean_commit", false},
		{"late_changed_branch", false},
		{"late_borrower", false},
		{"late_artifact_fd", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			city, rig := initReapRig(t)
			// Deterministic complete descriptor snapshot for this isolated fixture.
			// Strict host-wide lsof behavior is covered separately and live.
			oldHandles := archiveHandleSnapshot
			archiveHandleSnapshot = func() ([]byte, error) {
				return []byte(fmt.Sprintf("p%d\x00fcwd\x00tDIR\x00ar\x00n%s\x00", os.Getpid(), rig)), nil
			}
			t.Cleanup(func() { archiveHandleSnapshot = oldHandles })
			const id = "ga-life01"
			writeFileAt(t, rig, ".gitignore", ".codex/\n.agents/\n.claude/\n.gc/\n.mcp.json\n.runtime/\nsupabase/.temp/\nnode_modules/\n")
			mustGit(t, rig, "add", ".gitignore")
			mustGit(t, rig, "-c", "commit.gpgsign=false", "commit", "-m", "ignore runtime state")
			mustGit(t, rig, "push", "origin", "main")
			leaf := id
			if tc.name == "legacy_slug_task" {
				leaf += "-historical-task-slug"
			}
			home := "tasks"
			switch tc.name {
			case "crew_namespace":
				home = "crew"
			case "nested_task_namespace":
				home = filepath.Join("tasks", id)
				leaf = "worktree"
			case "prefix_collision_namespace":
				leaf = "prefix-" + id
			}
			wt := addClosedWorktree(t, rig, city, home, leaf)
			branch, err := newGitProbe(wt).CurrentBranch()
			if err != nil {
				t.Fatal(err)
			}
			writeFileAt(t, wt, "feature.txt", "first change\n")
			mustGit(t, wt, "add", "feature.txt")
			mustGit(t, wt, "-c", "commit.gpgsign=false", "commit", "-m", "feature one")
			writeFileAt(t, wt, "feature.txt", "first change\nsecond change\n")
			mustGit(t, wt, "add", "feature.txt")
			mustGit(t, wt, "-c", "commit.gpgsign=false", "commit", "-m", "feature two")
			head := productionLifecycleGitOutput(t, wt, "rev-parse", "HEAD")
			mustGit(t, wt, "push", "origin", branch)
			mustGit(t, rig, "merge", "--squash", branch)
			mustGit(t, rig, "-c", "commit.gpgsign=false", "commit", "-m", "squash merged feature")
			mustGit(t, rig, "push", "origin", "main")
			mustGit(t, rig, "push", "origin", "--delete", branch)
			mustGit(t, wt, "fetch", "--prune", "origin")
			merged := productionLifecycleGitOutput(t, rig, "rev-parse", "origin/main")
			if merged == head {
				t.Fatal("fixture did not rewrite the two topic commits")
			}
			if got := productionLifecycleGitOutput(t, rig, "show", "origin/main:feature.txt"); got != "first change\nsecond change" {
				t.Fatalf("squash failed to land feature: %q", got)
			}

			provider := "codex"
			if tc.name == "merged_claude_task" {
				provider = "claude"
			}
			if tc.name == "merged_runtime_staged_task" {
				data, err := core.PackFS.ReadFile("overlay/per-provider/codex/.codex/hooks.json")
				if err != nil {
					t.Fatal(err)
				}
				overlayRoot := filepath.Join(city, "runtime-overlay")
				writeFileAt(t, overlayRoot, "per-provider/codex/.codex/hooks.json", string(data))
				var warnings bytes.Buffer
				if err := runtime.StageSessionWorkDirWithWarnings(runtime.Config{WorkDir: wt, ProviderName: "codex", PackOverlayDirs: []string{overlayRoot}, CodexSessionFlags: nil}, &warnings); err != nil {
					t.Fatalf("actual startup staging: %v: %s", err, warnings.String())
				}
			} else if err := hooks.Install(fsys.OSFS{}, city, wt, []string{provider}); err != nil {
				t.Fatal(err)
			}
			hookTarget := filepath.Join(wt, ".codex", "hooks.json")
			if provider == "claude" {
				hookTarget = filepath.Join(city, ".gc", "settings.json")
			}
			if _, err := os.Stat(hookTarget); err != nil {
				t.Fatalf("actual hooks missing: %v", err)
			}
			if tc.name == "preexisting_mcp_config" {
				writeFileAt(t, wt, ".codex/config.toml", "model = \"user-selected-model\"\n")
			}
			projection, err := materialize.BuildMCPProjection(provider, wt, []materialize.MCPServer{{Name: "fixture", Transport: "stdio", Command: "fixture-tool"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := projection.Apply(fsys.OSFS{}); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(city, "skills", "lifecycle")
			writeSkillSource(t, source)
			shared := materialize.CityCatalog{OwnedRoots: []string{filepath.Dir(source)}, Entries: []materialize.SkillEntry{{Name: "lifecycle", Source: source, Origin: "city"}}}
			cfg := reapTestConfig(rig)
			var output bytes.Buffer
			if err := materializeSkillsIntoWorkdir(cfg, &config.Agent{Name: "builder", Provider: provider}, city, wt, &shared, &output, &output); err != nil {
				t.Fatal(err)
			}
			skillDir := ".agents"
			if provider == "claude" {
				skillDir = ".claude"
			}
			if info, err := os.Lstat(filepath.Join(wt, skillDir, "skills", "lifecycle")); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("actual skill link missing: %v", err)
			}
			rel, err := filepath.Rel(wt, projection.Target)
			if err != nil {
				t.Fatal(err)
			}
			artifacts := []string{filepath.ToSlash(rel)}
			if provider == "codex" {
				artifacts = append(artifacts, ".codex/hooks.json")
			}
			if tc.name == "preexisting_mcp_config" {
				// Real MCP adoption preserves the original config in a timestamped
				// backup. Explicitly archive that user content too, never all of .gc.
				backups, err := filepath.Glob(filepath.Join(wt, ".gc", "mcp-adopted", "codex", "*.toml"))
				if err != nil || len(backups) != 1 {
					t.Fatalf("expected one actual MCP adoption backup: %v %v", backups, err)
				}
				original, err := os.ReadFile(backups[0])
				if err != nil || string(original) != "model = \"user-selected-model\"\n" {
					t.Fatalf("MCP adoption did not preserve original user config: %q %v", original, err)
				}
				backup, err := filepath.Rel(wt, backups[0])
				if err != nil {
					t.Fatal(err)
				}
				artifacts = append(artifacts, filepath.ToSlash(backup))
			}
			meta := map[string]string{"gc.task_worktree": wt, "gc.work_dir": wt, "gc.work_branch": branch}
			live := liveWorktreeState{scanned: true}
			switch tc.name {
			case "custom_provider_config":
				raw, err := os.ReadFile(hookTarget)
				if err != nil {
					t.Fatal(err)
				}
				var doc map[string]any
				if err := json.Unmarshal(raw, &doc); err != nil {
					t.Fatal(err)
				}
				doc["user_owned_setting"] = "preserve this setting"
				raw, err = json.Marshal(doc)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(hookTarget, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			case "edited_mcp_config":
				raw, err := os.ReadFile(projection.Target)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(projection.Target, append(raw, []byte("\n# user-authored annotation\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
			case "handoff_note":
				writeFileAt(t, wt, ".runtime/handoff.md", "finished task handoff: preserve these notes\n")
				artifacts = append(artifacts, ".runtime/handoff.md")
			case "extensionless_cli_version":
				writeFileAt(t, wt, "supabase/.temp/cli-latest", "v2.75.0\n")
				artifacts = append(artifacts, "supabase/.temp/cli-latest")
			case "tracked_edit_after_merge":
				writeFileAt(t, wt, "feature.txt", "uncommitted user source\n")
			case "untracked_user_file":
				writeFileAt(t, wt, "local-source.ts", "unfinished source\n")
			case "unknown_ignored_source":
				writeFileAt(t, wt, ".codex/scratch.py", "unfinished ignored source\n")
			case "nested_repository_in_cache":
				mustGit(t, "", "init", filepath.Join(wt, "node_modules", "nested"))
			case "held_descriptor":
				archiveHandleSnapshot = func() ([]byte, error) {
					return []byte(fmt.Sprintf("p%d\x00fcwd\x00tDIR\x00ar\x00n%s\x00p%d\x00f5\x00tREG\x00au\x00n%s\x00", os.Getpid(), rig, os.Getpid()+1, filepath.Join(wt, "feature.txt"))), nil
				}
			case "unknown_descriptor":
				archiveHandleSnapshot = func() ([]byte, error) {
					return []byte(fmt.Sprintf("p%d\x00f10\x00a \x00n(revoked)\x00", os.Getpid())), nil
				}
			case "transient_poisoned_index":
				source := filepath.Join(wt, "feature.txt")
				aged := time.Now().Add(-time.Hour).Truncate(time.Second)
				if err := os.Chtimes(source, aged, aged); err != nil {
					t.Fatal(err)
				}
				mustGit(t, wt, "update-index", "--refresh")
				if err := os.WriteFile(source, []byte("First change\nsecond change\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(source, aged, aged); err != nil {
					t.Fatal(err)
				}
				if raw := productionLifecycleGitOutput(t, wt, "-c", "core.trustctime=false", "status", "--porcelain=v1"); raw != "" {
					t.Fatalf("poison fixture visible: %q", raw)
				}
				if raw := productionLifecycleGitOutput(t, wt, "status", "--porcelain=v1"); raw != "" {
					t.Fatalf("poison did not persist: %q", raw)
				}
			case "user_paused":
				meta["gc.user_paused"] = "true"
			case "live_process":
				live.cwds = []string{pathutil.NormalizePathForCompare(wt)}
			}
			rows := []beads.Bead{{ID: id, Status: "open", Metadata: meta}}
			if tc.name == "open_borrower" {
				rows = append(rows, beads.Bead{ID: "ga-borrow02", Status: "open", Metadata: map[string]string{"gc.work_dir": wt}})
			}
			store := beads.NewMemStoreFrom(1, rows, nil)
			scope := reapTestScope(map[string]beads.Store{reapTestRigName: store})
			if tc.name == "city_borrower" {
				scope.CityStore = beads.NewMemStoreFrom(1, []beads.Bead{{ID: "city-borrow02", Status: "open", Metadata: map[string]string{"work_dir": wt}}}, nil)
			}
			injectLiveness(t, live)
			evidence := []byte(`{"fixture":"exact two-commit squash merged into main"}`)
			evidencePath := filepath.Join(t.TempDir(), "completion.json")
			if err := os.WriteFile(evidencePath, evidence, 0o600); err != nil {
				t.Fatal(err)
			}
			pointer, err := os.Lstat(filepath.Join(wt, ".git"))
			if err != nil {
				t.Fatal(err)
			}
			device, inode, ok := fsys.FileIdentity(pointer)
			if !ok {
				t.Fatal("fixture pointer has no stable filesystem identity")
			}
			request := worktreeArchiveRequest{SchemaVersion: "1", Rig: reapTestRigName, BeadID: id, Path: wt, ExpectedHEAD: head, ExpectedBranch: "refs/heads/" + branch, ExpectedGitDir: productionLifecycleGitOutput(t, wt, "rev-parse", "--absolute-git-dir"), ExpectedPointerDevice: device, ExpectedPointerInode: inode, ProductionRef: "refs/remotes/origin/main", ExpectedProductionHEAD: merged, IntegratedCommit: merged, EvidencePath: evidencePath, EvidenceSHA256: fmt.Sprintf("%x", sha256.Sum256(evidence)), ArtifactPaths: artifacts}
			// Merge alone never closes work or authorizes its workspace removal.
			openResult := archiveReapWorktree(city, cfg, scope, nil, request, false, &output)
			if openResult.Outcome == "would_reap" || openResult.Outcome == "reaped" {
				t.Fatalf("open work admitted: %+v", openResult)
			}
			if _, err := os.Stat(wt); err != nil {
				t.Fatalf("open work lost: %v", err)
			}
			if err := store.Close(id); err != nil {
				t.Fatal(err)
			}
			closed, err := store.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			request.ExpectedBeadRevision = closed.Revision
			ordinary := reapClosedBeadWorktrees(city, cfg, scope, nil, false, events.Discard, nil, &output)
			if len(ordinary.Reaped) != 0 {
				t.Fatal("ordinary reaper discarded non-disposable config instead of requiring archival")
			}
			snapshot := map[string][]byte{}
			for _, path := range artifacts {
				data, err := os.ReadFile(filepath.Join(wt, path))
				if err != nil {
					t.Fatal(err)
				}
				snapshot[path] = data
			}
			preview := archiveReapWorktree(city, cfg, scope, nil, request, false, &output)
			if tc.protect {
				if tc.name == "held_descriptor" && (!preview.Complete || preview.ReasonCode != "open_handles") {
					t.Fatalf("held descriptor classification: %+v", preview)
				}
				if tc.name == "unknown_descriptor" && (preview.Complete || preview.ReasonCode != "handle_inventory_incomplete") {
					t.Fatalf("unknown descriptor classification: %+v", preview)
				}
				if preview.Outcome == "would_reap" || preview.Outcome == "reaped" {
					t.Fatalf("protected work admitted: %+v; %s", preview, output.String())
				}
				if preview.ArchivePath != "" {
					t.Fatalf("dry-run created archive: %+v", preview)
				}
				if _, err := os.Stat(wt); err != nil {
					t.Fatalf("protected work lost: %v", err)
				}
				if tc.name == "transient_poisoned_index" {
					data, err := os.ReadFile(filepath.Join(wt, "feature.txt"))
					if err != nil || string(data) != "First change\nsecond change\n" {
						t.Fatalf("poisoned source not preserved: %q %v", data, err)
					}
				}
			} else {
				if !preview.OK || !preview.Complete || preview.Outcome != "would_reap" || preview.InventorySHA256 == "" {
					t.Fatalf("finished task not eligible for verified archival: %+v; %s", preview, output.String())
				}
				if preview.ArchivePath != "" {
					t.Fatalf("plan created archive: %+v", preview)
				}
				checkpointRan := false
				expectedBranchHead := head
				var lateWriter *os.File
				if strings.HasPrefix(tc.name, "late_") {
					oldCheckpoint := archiveRetirementCheckpoint
					t.Cleanup(func() { archiveRetirementCheckpoint = oldCheckpoint })
					archiveRetirementCheckpoint = func(stage, original, quarantine string) {
						if tc.name == "late_artifact_fd" && stage == "before_move" {
							var err error
							lateWriter, err = os.OpenFile(filepath.Join(original, ".codex", "config.toml"), os.O_WRONLY, 0)
							if err != nil {
								t.Fatal(err)
							}
							t.Cleanup(func() { _ = lateWriter.Close() })
						}
						if (tc.name == "late_original_recreation" || tc.name == "late_recreated_original_with_hold") && stage == "after_move" {
							checkpointRan = true
							writeFileAt(t, original, "new-occupant.txt", "new namespace must survive\n")
						}
						if stage != "before_remove" || tc.name == "late_original_recreation" {
							return
						}
						checkpointRan = true
						switch tc.name {
						case "late_unknown_quarantine_leaf", "late_recreated_original_with_hold":
							writeFileAt(t, quarantine, ".runtime/new-source.py", "late unfinished source\n")
						case "late_clean_commit":
							writeFileAt(t, quarantine, "late-source.txt", "new committed source\n")
							mustGit(t, quarantine, "add", "late-source.txt")
							mustGit(t, quarantine, "-c", "commit.gpgsign=false", "commit", "-m", "late new work")
							expectedBranchHead = productionLifecycleGitOutput(t, quarantine, "rev-parse", "HEAD")
							if expectedBranchHead == head {
								t.Fatal("late commit did not change HEAD")
							}
						case "late_changed_branch":
							mustGit(t, quarantine, "checkout", "-b", "new-owner-branch")
						case "late_borrower":
							if _, err := scope.CityStore.Create(beads.Bead{Status: "open", Metadata: map[string]string{"gc.work_dir": original}}); err != nil {
								t.Fatal(err)
							}
						case "late_artifact_fd":
							if _, err := lateWriter.WriteAt([]byte("# late user bytes"), 0); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
				request.ExpectedInventorySHA256 = preview.InventorySHA256
				result := archiveReapWorktree(city, cfg, scope, nil, request, true, &output)
				if strings.HasPrefix(tc.name, "late_") && !checkpointRan {
					t.Fatal("retirement race checkpoint was not reached")
				}
				if strings.HasPrefix(tc.name, "late_") && tc.name != "late_original_recreation" {
					if result.Outcome == "reaped" || result.Outcome == "would_reap" {
						t.Fatalf("late work was removed: %+v", result)
					}
					if result.Complete || result.QuarantinePath == "" || result.ArchivePath == "" {
						t.Fatalf("uncertain retirement must retain its private checkout and archive: %+v", result)
					}
					retained := result.QuarantinePath
					if tc.name == "late_recreated_original_with_hold" {
						data, err := os.ReadFile(filepath.Join(wt, "new-occupant.txt"))
						if err != nil || string(data) != "new namespace must survive\n" {
							t.Fatalf("failed retirement touched recreated original: %q %v", data, err)
						}
					} else if _, err := os.Lstat(wt); !os.IsNotExist(err) {
						t.Fatalf("failed retirement unexpectedly recreated original path: %v", err)
					}
					if _, err := os.Stat(retained); err != nil {
						t.Fatalf("late work namespace lost: %v", err)
					}
					switch tc.name {
					case "late_unknown_quarantine_leaf", "late_recreated_original_with_hold":
						data, err := os.ReadFile(filepath.Join(retained, ".runtime", "new-source.py"))
						if err != nil || string(data) != "late unfinished source\n" {
							t.Fatalf("unknown quarantine source lost: %q %v", data, err)
						}
					case "late_clean_commit":
						if got := productionLifecycleGitOutput(t, rig, "rev-parse", "refs/heads/"+branch); got != expectedBranchHead {
							t.Fatal("late commit reference lost")
						}
					case "late_changed_branch":
						if got := productionLifecycleGitOutput(t, retained, "symbolic-ref", "--short", "HEAD"); got != "new-owner-branch" {
							t.Fatal("late branch change lost")
						}
					case "late_artifact_fd":
						data, err := os.ReadFile(filepath.Join(result.ArchivePath, "original-inodes", ".codex", "config.toml"))
						if err != nil || !bytes.HasPrefix(data, []byte("# late user bytes")) {
							t.Fatalf("late FD bytes not retained: %q %v", data, err)
						}
						archivedCopy, err := os.ReadFile(filepath.Join(result.ArchivePath, "data", ".codex", "config.toml"))
						if err != nil || !bytes.Equal(archivedCopy, snapshot[".codex/config.toml"]) {
							t.Fatal("immutable archive copy changed")
						}
					}
					return
				}
				if !result.OK || !result.Complete || result.Outcome != "reaped" {
					t.Fatalf("archive/reap failed: %+v; %s", result, output.String())
				}
				if tc.name == "late_original_recreation" {
					data, err := os.ReadFile(filepath.Join(wt, "new-occupant.txt"))
					if err != nil || string(data) != "new namespace must survive\n" {
						t.Fatalf("new original-path occupant was touched: %q %v", data, err)
					}
				} else if _, err := os.Stat(wt); !os.IsNotExist(err) {
					t.Fatalf("finished checkout remains: %v", err)
				}
				manifest, err := os.ReadFile(filepath.Join(result.ArchivePath, "manifest.json"))
				if err != nil {
					t.Fatal(err)
				}
				if got := fmt.Sprintf("%x", sha256.Sum256(manifest)); got != result.ManifestSHA256 {
					t.Fatalf("manifest hash mismatch: %s != %s", got, result.ManifestSHA256)
				}
				restored := t.TempDir()
				for path, want := range snapshot {
					archived := filepath.Join(result.ArchivePath, "data", path)
					got, err := os.ReadFile(archived)
					if err != nil || !bytes.Equal(got, want) {
						t.Fatalf("artifact not preserved %s: %v", path, err)
					}
					info, err := os.Stat(archived)
					if err != nil || info.Mode().Perm() != 0o600 {
						t.Fatalf("archive not private %s: %v", path, err)
					}
					writeFileAt(t, restored, path, string(got))
					back, err := os.ReadFile(filepath.Join(restored, path))
					if err != nil || !bytes.Equal(back, want) {
						t.Fatalf("artifact restoration failed %s: %v", path, err)
					}
				}
			}
			if got := productionLifecycleGitOutput(t, rig, "rev-parse", "refs/heads/"+branch); got != head {
				t.Fatalf("local branch lost: %s != %s", got, head)
			}
			if _, err := os.Stat(filepath.Join(source, "SKILL.md")); err != nil {
				t.Fatalf("external skill target lost: %v", err)
			}
		})
	}
}

func productionLifecycleGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(gitStdout(t, dir, args...))
}
