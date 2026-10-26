//go:build darwin || linux

package worktree

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupPreservesSourceHiddenByTransientStatCache(t *testing.T) {
	assertRetirementPreservesPoisonedSource(t, false)
}

func TestRollbackAttemptPreservesSourceHiddenByTransientStatCache(t *testing.T) {
	assertRetirementPreservesPoisonedSource(t, true)
}

func assertRetirementPreservesPoisonedSource(t *testing.T, rollback bool) {
	t.Helper()
	repo, base := initTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("AAAA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "source.txt")
	runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "-m", "committed source")
	root := t.TempDir()
	wt := filepath.Join(root, "task")
	spec := managedSpec(repo, root, wt, "work/content", base)
	ensured, err := Ensure(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.AttemptID = ensured.Provenance.AttemptID
	source := filepath.Join(wt, "source.txt")
	aged := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(source, aged, aged); err != nil {
		t.Fatal(err)
	}
	runGit(t, wt, "update-index", "--refresh")
	if err := os.WriteFile(source, []byte("BBBB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(source, aged, aged); err != nil {
		t.Fatal(err)
	}
	if raw := runGit(t, wt, "-c", "core.trustctime=false", "status", "--porcelain"); raw != "" {
		t.Fatalf("poison fixture visible: %q", raw)
	}
	if raw := runGit(t, wt, "status", "--porcelain"); raw != "" {
		t.Fatalf("false-clean control failed: %q", raw)
	}
	if rollback {
		if err := RollbackAttempt(spec, ensured); err == nil {
			t.Fatal("rollback admitted source hidden by poisoned stat cache")
		}
	} else {
		report, err := Cleanup(spec)
		if err == nil || report.Removed || report.Error == nil || report.Error.Code != CleanupErrorDirty {
			t.Fatalf("hidden source admitted: %+v %v", report, err)
		}
	}
	actual, err := os.ReadFile(source)
	if err != nil || string(actual) != "BBBB\n" {
		t.Fatalf("source lost: %q %v", actual, err)
	}
}

func TestNonforceRetirementPreservesIgnoredConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name           string
		rollback, hide bool
	}{
		{"cleanup/default", false, false},
		{"cleanup/hidden", false, true},
		{"rollback/default", true, false},
		{"rollback/hidden", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base := initTestRepo(t)
			root := t.TempDir()
			wt := filepath.Join(root, "task")
			spec := managedSpec(repo, root, wt, "work/ignored-config", base)
			created, err := Ensure(spec)
			if err != nil {
				t.Fatal(err)
			}
			spec.AttemptID = created.Provenance.AttemptID
			if tc.hide {
				runGit(t, wt, "config", "status.showUntrackedFiles", "no")
			}
			if err := os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte(".env.local\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(wt, ".env.local")
			const contents = "LOCAL_SETTING=synthetic-user-configuration\n"
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := runGit(t, wt, "status", "--porcelain"); got != "" {
				t.Fatalf("ordinary status unexpectedly sees ignored fixture: %q", got)
			}
			if got := runGit(t, wt, "check-ignore", "--", ".env.local"); got != ".env.local" {
				t.Fatalf("fixture is not Git-ignored: %q", got)
			}
			if tc.rollback {
				if err := RollbackAttempt(spec, created); err == nil {
					t.Fatal("rollback admitted ignored user configuration")
				}
			} else {
				report, err := Cleanup(spec)
				if err == nil || report.Removed || report.Error == nil || report.Error.Code != CleanupErrorDirty {
					t.Fatalf("cleanup admitted ignored user configuration: %+v %v", report, err)
				}
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != contents {
				t.Fatalf("ignored configuration lost: %q %v", got, err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("ignored configuration permissions changed: %v", err)
			}
			verified, err := Verify(spec)
			if err != nil || verified.Provenance == nil || verified.Provenance.AttemptID != created.Provenance.AttemptID {
				t.Fatalf("attempt provenance changed: %+v %v", verified, err)
			}
			if head := runGit(t, repo, "rev-parse", "refs/heads/"+spec.Branch); head != created.Head {
				t.Fatalf("attempt branch changed: %s != %s", head, created.Head)
			}
		})
	}
}
