//go:build !windows

package worktree

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTryLockRetirementSharesProvisioningLock(t *testing.T) {
	repo, _ := initTestRepo(t)
	root := t.TempDir()
	path := filepath.Join(root, "task")
	held, err := lockPath(repo, path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.unlock()
	start := time.Now()
	unlock, err := TryLockRetirement(repo, path)
	if unlock != nil {
		unlock()
		t.Fatal("busy acquisition returned unlock handle")
	}
	if !errors.Is(err, ErrRetirementLockBusy) {
		t.Fatalf("got %v, want busy", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("busy acquisition blocked")
	}
	held.unlock()
	unlock, err = TryLockRetirement(repo, path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	lockFile, err := lockFilePath(repo, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := tryFlock(t, lockFile); err == nil {
		t.Fatal("retirement lock failed to exclude existing provisioning namespace")
	}
	unlock()
	unlock()
	if err := tryFlock(t, lockFile); err != nil {
		t.Fatalf("unlock did not release canonical lock: %v", err)
	}
}

func TestTryLockRetirementIgnoresAmbientGitAndResolvesAliases(t *testing.T) {
	repo, _ := initTestRepo(t)
	other, _ := initTestRepo(t)
	root := t.TempDir()
	physicalRoot := filepath.Join(root, "real")
	if err := os.Mkdir(physicalRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(physicalRoot, alias); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(physicalRoot, "task")
	held, err := lockPath(repo, path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.unlock()
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(other, ".git"))
	unlock, err := TryLockRetirement(repo, filepath.Join(alias, "task"))
	if unlock != nil {
		unlock()
	}
	if !errors.Is(err, ErrRetirementLockBusy) {
		t.Fatalf("ambient or alias split canonical lock: %v", err)
	}
}
