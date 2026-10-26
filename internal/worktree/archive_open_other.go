//go:build !unix

package worktree

import (
	"errors"
	"os"
)

func archiveOpenRegular(_ *os.Root, _ string) (*os.File, error) {
	return nil, errors.New("private archive stable reads unsupported on this platform")
}

func archiveRetainOriginal(_ *os.Root, _ string, _ string, _ string, _ os.FileInfo) error {
	return errors.New("inode retention unsupported")
}

func archiveOwned(_ os.FileInfo) bool { return false }

func archiveInodeIdentity(_ os.FileInfo) string { return "" }
