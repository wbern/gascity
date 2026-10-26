//go:build !linux && !darwin

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

var errWorktreeRetirementPlatform = errors.New("guarded worktree retirement is unsupported on this platform")

func archiveInventoryCommand(context.Context, string, ...string) (*exec.Cmd, error) {
	return nil, errWorktreeRetirementPlatform
}

func workerDirRetryPlatformSupported() error                     { return errWorktreeRetirementPlatform }
func workerDirRetryOwned(os.FileInfo) bool                       { return false }
func workerDirRetryLinkCount(os.FileInfo) uint64                 { return 0 }
func sameWorkerDirRetryOwnerLinks(os.FileInfo, os.FileInfo) bool { return false }
func openWorkerDirRetryPlatformFile(*os.Root, string, int, os.FileMode) (*os.File, error) {
	return nil, errWorktreeRetirementPlatform
}
