//go:build darwin || linux

package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyTrackedContentBlobFormatsAndSymlinkTargets(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			repo := t.TempDir()
			runGit(t, repo, "init", "--object-format="+format)
			runGit(t, repo, "config", "user.name", "fixture")
			runGit(t, repo, "config", "user.email", "fixture@example.invalid")
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("outside data"), 0o000); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("tracked bytes\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(repo, "link")); err != nil {
				t.Fatal(err)
			}
			runGit(t, repo, "add", "source.txt", "link")
			runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "-m", "tracked content fixture")
			head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
			if err := NewInspection(repo).VerifyTrackedContent(head); err != nil {
				t.Fatalf("clean content or unfollowed symlink rejected: %v", err)
			}
			if err := os.Remove(filepath.Join(repo, "link")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside+"-changed", filepath.Join(repo, "link")); err != nil {
				t.Fatal(err)
			}
			if err := NewInspection(repo).VerifyTrackedContent(head); err == nil {
				t.Fatal("changed symlink target admitted")
			}
		})
	}
}

func TestVerifyTrackedContentPreservesTransformedWorkingBytes(t *testing.T) {
	repo := initTestRepo(t)
	file := filepath.Join(repo, "source.txt")
	if err := os.WriteFile(file, []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "source.txt")
	runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "-m", "LF canonical source")
	head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	if err := NewInspection(repo).VerifyTrackedContent(head); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "config", "core.autocrlf", "true")
	if err := os.WriteFile(file, []byte("source\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewInspection(repo).VerifyTrackedContent(head); err == nil {
		t.Fatal("noncanonical working bytes admitted")
	}
	actual, err := os.ReadFile(file)
	if err != nil || string(actual) != "source\r\n" {
		t.Fatalf("working bytes changed: %q %v", actual, err)
	}
}

func TestVerifyTrackedContentUsesGitOwnerExecutableBit(t *testing.T) {
	repo := initTestRepo(t)
	source := filepath.Join(repo, "source.sh")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "source.sh")
	runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "-m", "executable source")
	head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	if err := NewInspection(repo).VerifyTrackedContent(head); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "config", "core.filemode", "false")
	if err := os.Chmod(source, 0o645); err != nil {
		t.Fatal(err)
	}
	if raw := runGit(t, repo, "status", "--porcelain"); raw != "" {
		t.Fatalf("filemode fixture visible to ordinary status: %q", raw)
	}
	if diff := runGit(t, repo, "-c", "core.filemode=true", "diff", "--summary"); !strings.Contains(diff, "100755 => 100644") {
		t.Fatalf("Git owner-bit change not reproduced: %q", diff)
	}
	if err := NewInspection(repo).VerifyTrackedContent(head); err == nil {
		t.Fatal("owner executable bit loss admitted because another execute bit remained")
	}
	info, err := os.Stat(source)
	if err != nil || info.Mode().Perm() != 0o645 {
		t.Fatalf("inspection changed file mode: %v %v", info, err)
	}
}
