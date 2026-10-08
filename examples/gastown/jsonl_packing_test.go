package gastown_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestJsonlOrderCoversPackingBudget(t *testing.T) {
	var config struct{ Order struct{ Timeout string } }
	if _, err := toml.DecodeFile(filepath.Join(corePackDir(), "orders", "jsonl-export.toml"), &config); err != nil {
		t.Fatal(err)
	}
	bound, err := time.ParseDuration(config.Order.Timeout)
	// Two 60s integrity checks + 300s packing + 10s kill grace leave normal
	// export/push time inside the documented 15m order budget.
	if err != nil || bound < 15*time.Minute {
		t.Fatalf("JSONL order timeout %q cannot cover packing and export: %v", config.Order.Timeout, err)
	}
}

// Packing must share export ownership, preserve refs and dirty files, and fail
// closed when Git or its connectivity verification fails.
func TestJSONLArchivePacking(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"clean", "noop", "tiny", "dirty", "busy", "low-free", "timeout-failure", "missing-flock-failure", "pack-failure", "fsck-failure", "refs-failure"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			repo := filepath.Join(dir, "archive")
			initSeedArchive(t, repo, 1)
			if scenario == "noop" {
				runGit(t, repo, "gc", "--no-prune")
			}
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			harness := filepath.Join(dir, "run.sh")
			writeExecutable(t, harness, `#!/usr/bin/env bash
set -euo pipefail
. "$HELPER"
read_state_json() { [ ! -f "$STATE" ] && echo '{}' || cat "$STATE"; }
write_state_json() { printf '%s\n' "$1" > "$STATE"; }
archive_lock || exit $?
archive_pack_if_due
git -C "$ARCHIVE_REPO" fsck --connectivity-only --no-dangling
`)
			// The shim is a recording boundary for the two failure edges; all
			// setup and happy-path commands execute real Git.
			writeExecutable(t, filepath.Join(bin, "git"), `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$GIT_LOG"
case " $* " in
  *' gc --no-prune '*) [ "$SCENARIO" != pack-failure ] || exit 42 ;;
  *' fsck '*) [ "$SCENARIO" != fsck-failure ] || exit 43 ;;
  *' show-ref '*) [ "$SCENARIO" != refs-failure ] || exit 44 ;;
esac
exec "$REAL_GIT" "$@"
`)
			if scenario == "dirty" {
				if err := os.Chtimes(filepath.Join(repo, "beads", "issues.jsonl"), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, "unlanded"), []byte("retain me"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			originalIndex, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
			if err != nil {
				t.Fatal(err)
			}
			env := map[string]string{
				"HELPER":       coreScriptPath("jsonl-archive-maintenance.sh"),
				"ARCHIVE_REPO": repo, "STATE": filepath.Join(dir, "state.json"),
				"GC_JSONL_PACK_INTERVAL_SEC": "21600", "GC_JSONL_PACK_MIN_FREE_MB": "1",
				"GC_JSONL_PACK_MIN_LOOSE_MB": "0",
				"SCENARIO":                   scenario, "GIT_LOG": filepath.Join(dir, "git.log"),
				"REAL_GIT": realGit, "PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			}
			if scenario == "tiny" {
				env["GC_JSONL_PACK_MIN_LOOSE_MB"] = "256"
			}
			if scenario == "low-free" {
				writeExecutable(t, filepath.Join(bin, "df"), "#!/bin/sh\nprintf 'Filesystem 1024-blocks Used Available Capacity Mounted\\nfixture 100 99 1 99%% /\\n'\n")
			}
			if scenario == "timeout-failure" {
				realTimeout, err := exec.LookPath("timeout")
				if err != nil {
					realTimeout, err = exec.LookPath("gtimeout")
				}
				if err != nil {
					t.Fatal(err)
				}
				env["REAL_TIMEOUT"] = realTimeout
				writeExecutable(t, filepath.Join(bin, "timeout"), `#!/usr/bin/env bash
if [ "$1" = --kill-after=10 ]; then
  [ "$2" = 300 ] || exit 98
  exit 124
fi
exec "$REAL_TIMEOUT" "$@"
`)
			}
			if scenario == "missing-flock-failure" {
				bash, err := exec.LookPath("bash")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(bash, filepath.Join(bin, "bash")); err != nil {
					t.Fatal(err)
				}
				env["PATH"] = bin
			}
			if scenario == "busy" {
				writeExecutable(t, harness, `#!/usr/bin/env bash
set -euo pipefail
. "$HELPER"
archive_lock
# Kernel lock inherited by this process proves overlap without sleeps.
flock -n "${ARCHIVE_REPO}.export.lock" true && exit 1
echo busy-proven
`)
			}
			out, err := runScriptResult(t, harness, env)
			if strings.HasSuffix(scenario, "failure") {
				if err == nil {
					t.Fatalf("%s falsely succeeded: %s", scenario, out)
				}
				want := "exit status 42"
				if scenario == "fsck-failure" {
					want = "exit status 1"
					if !strings.Contains(string(out), "pre-packing connectivity failed") {
						t.Fatalf("wrong guard failed: %s", out)
					}
				}
				if scenario == "refs-failure" {
					want = "exit status 1"
					if !strings.Contains(string(out), "cannot read archive references") {
						t.Fatal(string(out))
					}
				}
				if scenario == "timeout-failure" {
					want = "exit status 124"
					if !strings.Contains(string(out), "packing failed rc=124") {
						t.Fatal(string(out))
					}
				}
				if scenario == "missing-flock-failure" {
					want = "exit status 1"
					if !strings.Contains(string(out), "flock is required") {
						t.Fatal(string(out))
					}
				}
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("wrong failure: %v %s", err, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v\n%s", scenario, err, out)
			}
			if scenario == "busy" {
				if !strings.Contains(string(out), "busy-proven") {
					t.Fatal(string(out))
				}
				return
			}
			log, err := os.ReadFile(env["GIT_LOG"])
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "tiny" || scenario == "low-free" {
				if strings.Contains(string(log), "gc --no-prune") {
					t.Fatal("guarded archive was packed")
				}
				if scenario == "low-free" && !strings.Contains(string(out), "insufficient temporary disk space") {
					t.Fatalf("wrong guard: %s", out)
				}
				return
			}
			if scenario == "dirty" {
				index, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
				if err != nil || string(index) != string(originalIndex) {
					t.Fatal("dirty preflight rewrote index")
				}
				if strings.Contains(string(log), "gc --no-prune") {
					t.Fatal("packed dirty export")
				}
				if data, err := os.ReadFile(filepath.Join(repo, "unlanded")); err != nil || string(data) != "retain me" {
					t.Fatal("dirty export changed")
				}
			} else {
				for _, required := range []string{"gc --no-prune", "pack.threads=2", "pack.windowMemory=64m", "pack.deltaCacheSize=32m", "fsck --full"} {
					if !strings.Contains(string(log), required) {
						t.Fatalf("missing %s: %s", required, log)
					}
				}
				state, err := os.ReadFile(env["STATE"])
				if err != nil || !strings.Contains(string(state), "pending_export") {
					t.Fatalf("missing next-export proof state: %s %v", state, err)
				}
				var record struct {
					Packing struct {
						RSS  int64 `json:"max_rss_kib"`
						Pre  int64 `json:"pre_kib"`
						Post int64 `json:"post_kib"`
					} `json:"packing"`
				}
				if err := json.Unmarshal(state, &record); err != nil {
					t.Fatal(err)
				}
				if record.Packing.RSS <= 0 {
					t.Fatalf("real time process did not measure RSS: %s", state)
				}
				if scenario == "noop" && record.Packing.Pre != record.Packing.Post {
					t.Fatalf("noop fixture unexpectedly changed storage: %s", state)
				}
			}
		})
	}
}

func TestJSONLBoundedPacking(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"clean", "minimum-git", "dirty", "low-free", "high-load", "missing-load", "pending-export", "bad-batch", "old-git", "unknown-git", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			repo := filepath.Join(dir, "archive")
			initSeedArchive(t, repo, 1)
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(dir, "state.json")
			loadFile := filepath.Join(dir, "loadavg")
			if err := os.WriteFile(loadFile, []byte("0.01 0.01 0.01 1/1 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			harness := filepath.Join(dir, "run.sh")
			writeExecutable(t, harness, `#!/usr/bin/env bash
set -euo pipefail
. "$HELPER"
read_state_json() { [ ! -f "$STATE" ] && echo '{}' || cat "$STATE"; }
write_state_json() { printf '%s\n' "$1" > "$STATE"; }
archive_lock
archive_pack_if_due
`)
			writeExecutable(t, filepath.Join(bin, "git"), `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$GIT_LOG"
if [ "$*" = --version ] && [ "${GIT_VERSION_OVERRIDE:-}" != "" ]; then
    printf 'git version %s\n' "$GIT_VERSION_OVERRIDE"
    exit 0
fi
exec "$REAL_GIT" "$@"
`)
			env := map[string]string{
				"HELPER":       coreScriptPath("jsonl-archive-maintenance.sh"),
				"ARCHIVE_REPO": repo, "STATE": state,
				"GC_JSONL_PACK_INTERVAL_SEC": "1", "GC_JSONL_PACK_MODE": "bounded",
				"GC_JSONL_PACK_MIN_FREE_MB": "1", "GC_JSONL_PACK_MIN_LOOSE_MB": "0",
				"GC_JSONL_PACK_BATCH_SIZE": "4", "GC_JSONL_PACK_LOADAVG_FILE": loadFile,
				"GC_JSONL_PACK_MAX_LOAD": "4",
				"REAL_GIT":               realGit, "GIT_LOG": filepath.Join(dir, "git.log"),
				"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			}
			switch scenario {
			case "minimum-git":
				env["GIT_VERSION_OVERRIDE"] = "2.50.0"
			case "dirty":
				if err := os.WriteFile(filepath.Join(repo, "uncommitted"), []byte("keep"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "low-free":
				writeExecutable(t, filepath.Join(bin, "df"), "#!/bin/sh\nprintf 'Filesystem 1024-blocks Used Available Capacity Mounted\\nfixture 100 99 1 99%% /\\n'\n")
			case "high-load":
				if err := os.WriteFile(loadFile, []byte("99.0 99.0 99.0 1/1 1\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "missing-load":
				env["GC_JSONL_PACK_LOADAVG_FILE"] = filepath.Join(dir, "absent-loadavg")
			case "pending-export":
				if err := os.WriteFile(state, []byte(`{"packing":{"pending_export":true}}`), 0o644); err != nil {
					t.Fatal(err)
				}
			case "bad-batch":
				env["GC_JSONL_PACK_BATCH_SIZE"] = "65"
			case "old-git":
				env["GIT_VERSION_OVERRIDE"] = "2.47.1"
			case "unknown-git":
				env["GIT_VERSION_OVERRIDE"] = "unknown"
			case "timeout":
				realTimeout, err := exec.LookPath("timeout")
				if err != nil {
					t.Fatal(err)
				}
				env["REAL_TIMEOUT"] = realTimeout
				writeExecutable(t, filepath.Join(bin, "timeout"), `#!/usr/bin/env bash
if [ "$1" = --kill-after=10 ] && [ "$2" = 180 ]; then exit 124; fi
exec "$REAL_TIMEOUT" "$@"
`)
			}
			out, err := runScriptResult(t, harness, env)
			if scenario == "timeout" || scenario == "bad-batch" || scenario == "old-git" || scenario == "unknown-git" {
				if err == nil {
					t.Fatalf("%s must fail closed: %s", scenario, out)
				}
				if scenario == "timeout" && !strings.Contains(err.Error(), "exit status 124") {
					t.Fatalf("bounded timeout must propagate: %v %s", err, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v\n%s", scenario, err, out)
			}
			if scenario != "clean" && scenario != "minimum-git" {
				packs, err := filepath.Glob(filepath.Join(repo, ".git", "objects", "pack", "*.pack"))
				if err != nil || len(packs) != 0 {
					t.Fatalf("%s unexpectedly packed archive: %v %v", scenario, packs, err)
				}
				return
			}
			data, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Packing struct {
					Mode        string `json:"mode"`
					BatchSize   int    `json:"batch_size"`
					Pending     bool   `json:"pending_export"`
					LooseBefore int    `json:"loose_before_count"`
					PacksBefore int    `json:"packs_before_count"`
					PacksAfter  int    `json:"packs_after_count"`
					FreeKiB     int64  `json:"free_kib"`
					RequiredKiB int64  `json:"required_kib"`
				} `json:"packing"`
			}
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if result.Packing.Mode != "bounded" || result.Packing.BatchSize != 4 || !result.Packing.Pending {
				t.Fatalf("missing bounded packing receipt: %s", data)
			}
			if result.Packing.LooseBefore == 0 || result.Packing.PacksAfter <= result.Packing.PacksBefore || result.Packing.RequiredKiB == 0 || result.Packing.FreeKiB < result.Packing.RequiredKiB {
				t.Fatalf("missing capacity/object-count proof: %s", data)
			}
			gitLog, err := os.ReadFile(env["GIT_LOG"])
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(gitLog), "maintenance run --task=loose-objects") || strings.Contains(string(gitLog), " gc --no-prune") {
				t.Fatalf("wrong Git maintenance task: %s", gitLog)
			}
		})
	}
}

func TestJsonlExportVerifiesPackingThroughNextExportAndPush(t *testing.T) {
	for _, scenario := range []string{"success", "bounded-success", "timeout-recovery", "export-failure", "empty-inventory", "push-failure"} {
		t.Run(scenario, func(t *testing.T) {
			city, bin, state := t.TempDir(), t.TempDir(), t.TempDir()
			repo := filepath.Join(city, "archive")
			remote, _ := initSeedArchiveWithRemote(t, repo)
			writeMultiRecordDoltStub(t, bin, 100)
			writeJsonlExportGCStub(t, bin)
			env := jsonlExportEnv(t, city, bin, state, repo, filepath.Join(city, "gc.log"), filepath.Join(city, "mail.log"))
			env["GC_JSONL_PACK_INTERVAL_SEC"] = "21600"
			env["GC_JSONL_PACK_MIN_FREE_MB"] = "1"
			env["GC_JSONL_PACK_MIN_LOOSE_MB"] = "0"
			if scenario == "bounded-success" {
				env["GC_JSONL_PACK_MODE"] = "bounded"
				env["GC_JSONL_PACK_BATCH_SIZE"] = "4"
			}
			if scenario == "export-failure" {
				writeIssuesPayloadDoltStub(t, bin, "malformed")
			}
			if scenario == "empty-inventory" {
				writeExecutable(t, filepath.Join(bin, "dolt"), "#!/bin/sh\nprintf 'Database\\n'\n")
			}
			if scenario == "push-failure" {
				realGit, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				writeGitSubcommandFailureStub(t, bin, realGit, "push")
			}
			if scenario == "timeout-recovery" {
				realTimeout, err := exec.LookPath("timeout")
				if err != nil {
					realTimeout, err = exec.LookPath("gtimeout")
				}
				if err != nil {
					t.Fatal(err)
				}
				env["REAL_TIMEOUT"] = realTimeout
				writeExecutable(t, filepath.Join(bin, "timeout"), `#!/usr/bin/env bash
if [ "$1" = --kill-after=10 ]; then exit 124; fi
exec "$REAL_TIMEOUT" "$@"
`)
			}
			out, err := runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
			if scenario == "timeout-recovery" {
				if err == nil || !strings.Contains(err.Error(), "exit status 124") {
					t.Fatalf("timeout not propagated: %v %s", err, out)
				}
				if err := os.Remove(filepath.Join(bin, "timeout")); err != nil {
					t.Fatal(err)
				}
				out, err = runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
			}
			success := scenario == "success" || scenario == "bounded-success" || scenario == "timeout-recovery"
			if success && err != nil {
				t.Fatalf("export after packing: %v %s", err, out)
			}
			if !success && err == nil {
				t.Fatalf("failed %s falsely verified: %s", scenario, out)
			}
			data, err := os.ReadFile(filepath.Join(state, "jsonl-export-state.json"))
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Packing struct {
					Pending  bool   `json:"pending_export"`
					Verified string `json:"export_verified_at"`
				} `json:"packing"`
			}
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if success {
				if result.Packing.Pending || result.Packing.Verified == "" {
					t.Fatalf("missing proof: %s", data)
				}
				if got, want := runGitOut(t, remote, "rev-parse", "refs/heads/main"), runGitOut(t, repo, "rev-parse", "HEAD"); got != want {
					t.Fatal("next export not pushed")
				}
			} else if !result.Packing.Pending || result.Packing.Verified != "" {
				t.Fatalf("failure cleared proof obligation: %s", data)
			}
		})
	}
}

func TestJsonlExportDefersBeforeReadingWhenArchiveLocked(t *testing.T) {
	city, bin, state := t.TempDir(), t.TempDir(), t.TempDir()
	repo := filepath.Join(city, "archive")
	initSeedArchive(t, repo, 1)
	writeMultiRecordDoltStub(t, bin, 1)
	writeJsonlExportGCStub(t, bin)
	env := jsonlExportEnv(t, city, bin, state, repo, filepath.Join(city, "gc.log"), filepath.Join(city, "mail.log"))
	env["ARCHIVE_REPO"] = repo
	env["HELPER"] = coreScriptPath("jsonl-archive-maintenance.sh")
	env["EXPORTER"] = coreScriptPath("jsonl-export.sh")
	env["DOLT_ARGS_LOG"] = filepath.Join(city, "dolt.log")
	harness := filepath.Join(city, "overlap.sh")
	writeExecutable(t, harness, `#!/usr/bin/env bash
set -euo pipefail
. "$HELPER"
archive_lock
"$EXPORTER"
`)
	out, err := runScriptResult(t, harness, env)
	if err != nil || !strings.Contains(string(out), "archive busy; deferred") {
		t.Fatalf("overlap: %v %s", err, out)
	}
	if log, err := os.ReadFile(env["DOLT_ARGS_LOG"]); err == nil && len(log) != 0 {
		t.Fatalf("overlapping export queried source: %s", log)
	}
	if got := runGitOut(t, repo, "status", "--porcelain"); got != "" {
		t.Fatalf("overlap dirtied archive: %s", got)
	}
}
