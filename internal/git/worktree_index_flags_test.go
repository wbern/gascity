package git

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Git status and non-force worktree removal both trust these index bits. A
// safety inspection must refuse even clean flagged files without clearing bits
// or rewriting source: otherwise later edits are silently invisible.
func TestWorkingTreeStatusProtectsHiddenIndexFlags(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		for _, modified := range []bool{false, true} {
			name := strings.TrimPrefix(flag, "--") + "/clean"
			if modified {
				name = strings.TrimPrefix(flag, "--") + "/modified"
			}
			t.Run(name, func(t *testing.T) {
				repo := initTestRepo(t)
				path := filepath.Join(repo, "source.txt")
				if err := os.WriteFile(path, []byte("committed source\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, repo, "add", "source.txt")
				runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "-m", "tracked fixture")
				control, err := NewInspection(repo).WorkingTreeStatus()
				if err != nil || len(control.Dirty) != 0 {
					t.Fatalf("ordinary clean control failed: %+v %v", control, err)
				}
				runGit(t, repo, "update-index", flag, "source.txt")
				expected := []byte("committed source\n")
				if modified {
					expected = []byte("uncommitted source must survive\n")
					if err := os.WriteFile(path, expected, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				before := runGit(t, repo, "ls-files", "-v", "-z")
				// Prove the real Git status false-negative that made the old guard unsafe.
				if raw := runGit(t, repo, "--no-optional-locks", "-c", "core.fsmonitor=false", "status", "--porcelain=v1", "-z", "--ignored"); raw != "" {
					t.Fatalf("fixture did not hide tracked content: %q", raw)
				}
				if !NewInspection(repo).HasUncommittedOrIgnoredWork() {
					t.Fatal("boolean inspection admitted hidden index flags")
				}
				_, err = NewInspection(repo).WorkingTreeStatus()
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), "index") {
					t.Fatalf("hidden index state not refused by inspection: %v", err)
				}
				if after := runGit(t, repo, "ls-files", "-v", "-z"); after != before {
					t.Fatalf("inspection changed index flags: before%q after%q", before, after)
				}
				actual, err := os.ReadFile(path)
				if err != nil || string(actual) != string(expected) {
					t.Fatalf("inspection changed source: %q %v", actual, err)
				}
			})
		}
	}
}

// A normal H index tag is insufficient: persisted trustctime=false can hide
// same-size source edits whose old mtime was restored, even without hidden bits.
func TestWorkingTreeStatusProtectsWeakStatCacheWithRestoredMtime(t *testing.T) {
	repo := initTestRepo(t)
	path := filepath.Join(repo, "source.txt")
	if err := os.WriteFile(path, []byte("AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "source.txt")
	runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "-m", "stat-cache fixture")
	aged := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, aged, aged); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "update-index", "--refresh")
	runGit(t, repo, "config", "--local", "core.trustctime", "false")
	expected := []byte("BBBB\n")
	if err := os.WriteFile(path, expected, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, aged, aged); err != nil {
		t.Fatal(err)
	}
	flags := runGit(t, repo, "ls-files", "-v", "-z")
	if flags != "H source.txt\x00" {
		t.Fatalf("fixture relies on hidden flags instead of weak stat cache: %q", flags)
	}
	if raw := runGit(t, repo, "--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "status", "--porcelain=v1", "-z", "--ignored"); raw != "" {
		t.Fatalf("weak-stat fixture failed to hide same-size edit: %q", raw)
	}
	indexPath := filepath.Join(repo, ".git", "index")
	configPath := filepath.Join(repo, ".git", "config")
	beforeIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !NewInspection(repo).HasUncommittedOrIgnoredWork() {
		t.Fatal("boolean inspection admitted weak stat cache")
	}
	_, err = NewInspection(repo).WorkingTreeStatus()
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "trustctime") {
		t.Fatalf("weak persisted stat cache was not refused: %v", err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("inspection changed source bytes: %q %v", actual, err)
	}
	afterIndex, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(afterIndex, beforeIndex) {
		t.Fatalf("inspection changed index: %v", err)
	}
	afterConfig, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(afterConfig, beforeConfig) {
		t.Fatalf("inspection changed persisted config: %v", err)
	}
	if after := runGit(t, repo, "ls-files", "-v", "-z"); after != flags {
		t.Fatalf("inspection changed flags: %q", after)
	}
}

// A transient command override can poison index stat metadata, leaving neither
// persisted weak settings nor hidden flags to explain Git's false clean result.
func TestWorkingTreeStatusProtectsTransientlyPoisonedStatCache(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_OPTIONAL_LOCKS", "1") // the poisoning command must refresh its index
	repo := initTestRepo(t)
	path := filepath.Join(repo, "source.txt")
	if err := os.WriteFile(path, []byte("AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "source.txt")
	runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "-m", "transient stat-cache fixture")
	aged := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, aged, aged); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "update-index", "--refresh")
	expected := []byte("BBBB\n")
	if err := os.WriteFile(path, expected, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, aged, aged); err != nil {
		t.Fatal(err)
	}
	if raw := runGit(t, repo, "-c", "core.trustctime=false", "status", "--porcelain=v1", "-z", "--ignored"); raw != "" {
		t.Fatalf("transient setting failed to poison stat cache: %q", raw)
	}
	if raw := runGit(t, repo, "--no-optional-locks", "status", "--porcelain=v1", "-z", "--ignored"); raw != "" {
		t.Fatalf("fixture did not preserve false-clean result after transient override: %q", raw)
	}
	flags := runGit(t, repo, "ls-files", "-v", "-z")
	if flags != "H source.txt\x00" {
		t.Fatalf("fixture unexpectedly relies on hidden flags: %q", flags)
	}
	settings, err := runGitAllowFail(t, repo, "config", "--show-scope", "--get-regexp", `^core\.(trustctime|checkstat|ignorestat|fsmonitor)$`)
	var missingKey *exec.ExitError
	if settings != "" || !errors.As(err, &missingKey) || missingKey.ExitCode() != 1 {
		t.Fatalf("fixture weak-setting absence not proven: %q %v", settings, err)
	}
	indexPath := filepath.Join(repo, ".git", "index")
	configPath := filepath.Join(repo, ".git", "config")
	beforeIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	expectedHEAD := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	state, inspectionErr := NewInspection(repo).WorkingTreeStatus()
	if inspectionErr != nil || len(state.Dirty) != 0 {
		t.Fatalf("cheap status no longer demonstrates the poisoned false-clean control: %+v %v", state, inspectionErr)
	}
	if err := NewInspection(repo).VerifyTrackedContent(expectedHEAD); err == nil {
		t.Fatal("content verification admitted source hidden by transiently poisoned index")
	}
	if !NewInspection(repo).HasUncommittedOrIgnoredWork() {
		t.Fatal("boolean inspection admitted transiently poisoned index")
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("inspection changed source bytes: %q %v", actual, err)
	}
	afterIndex, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(afterIndex, beforeIndex) {
		t.Fatalf("inspection changed index: %v", err)
	}
	afterConfig, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(afterConfig, beforeConfig) {
		t.Fatalf("inspection changed persisted config: %v", err)
	}
	if after := runGit(t, repo, "ls-files", "-v", "-z"); after != flags {
		t.Fatalf("inspection changed flags: %q", after)
	}
}

func TestWorkingTreeStatusOverridesHiddenUntrackedConfiguration(t *testing.T) {
	repo := initTestRepo(t)
	runGit(t, repo, "config", "status.showUntrackedFiles", "no")
	if err := os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte(".env.local\nignored-cache/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{".env.local": "SETTING=synthetic\n", "source/new.go": "package fixture\n", "ignored-cache/a.log": "a", "ignored-cache/b.log": "b"} {
		full := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if raw := runGit(t, repo, "status", "--porcelain=v1", "-z", "--ignored"); raw != "" {
		t.Fatalf("configured suppression did not hide fixture: %q", raw)
	}
	control := runGit(t, repo, "status", "--porcelain=v1", "-z", "--ignored", "--untracked-files=normal")
	for _, record := range []string{"?? source/\x00", "!! .env.local\x00", "!! ignored-cache/\x00"} {
		if !strings.Contains(control, record) {
			t.Fatalf("explicit normal control lacks %q: %q", record, control)
		}
	}
	status, err := NewInspection(repo).WorkingTreeStatus()
	ignored := make(map[string]bool)
	for _, path := range status.Ignored {
		ignored[path] = true
	}
	if err != nil || len(status.Dirty) != 1 || status.Dirty[0] != "source/" || len(status.Ignored) != 2 || !ignored[".env.local"] || !ignored["ignored-cache/"] {
		t.Fatalf("inspection hid source/config or expanded ignored cache leaves: %+v %v", status, err)
	}
	if !NewInspection(repo).HasUncommittedOrIgnoredWork() {
		t.Fatal("boolean guard accepted suppressed work")
	}
	if value := strings.TrimSpace(runGit(t, repo, "config", "status.showUntrackedFiles")); value != "no" {
		t.Fatalf("inspection changed user preference: %q", value)
	}
}
