//go:build !windows

package worktree

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
)

// TryLockRetirement takes the canonical Ensure/Cleanup path lock without
// waiting. It creates lock state, so only an explicit execute operation may
// call it. Hold the returned idempotent unlock through qualification, archival,
// quarantine, and removal; a busy result must leave the task protected.
func TryLockRetirement(repoDir, path string) (func(), error) {
	file, err := openRetirementLockFile(repoDir, path)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrRetirementLockBusy
		}
		return nil, fmt.Errorf("locking worktree retirement: %w", err)
	}
	held := &pathLock{f: file}
	var once sync.Once
	return func() { once.Do(held.unlock) }, nil
}
