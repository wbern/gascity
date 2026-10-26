package worktree

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/git"
)

// ErrRetirementLockBusy means an Ensure, Cleanup, or retirement currently owns
// this task path. Callers protect the task and retry on a later pass.
var ErrRetirementLockBusy = errors.New("worktree retirement lock busy")

// ErrRetirementLockUnsupported refuses retirement when nonblocking exclusion
// is not available on this platform.
var ErrRetirementLockUnsupported = errors.New("worktree retirement lock unsupported")

// openRetirementLockFile shares lockFilePath's common-dir namespace and
// canonicalLockKey exactly, but resolves the repository through inspection Git
// so ambient GIT_* cannot redirect a safety-critical retirement lock.
func openRetirementLockFile(repoDir, path string) (*os.File, error) {
	common, err := git.NewInspection(repoDir).CommonDir()
	if err != nil {
		return nil, fmt.Errorf("locating retirement lock directory: %w", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(canonicalLockKey(absolute)))
	lockFile := filepath.Join(common, "gc-worktree-locks", hex.EncodeToString(sum[:])+".lock")
	if err := os.MkdirAll(filepath.Dir(lockFile), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o644)
}
