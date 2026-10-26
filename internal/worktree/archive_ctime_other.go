//go:build !darwin && !linux

package worktree

import "os"

func archiveChangeStamp(_ os.FileInfo) string { return "" }
