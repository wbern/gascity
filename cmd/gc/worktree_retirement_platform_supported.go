//go:build linux || darwin

package main

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Keep process-group cancellation and inode ownership/link probes confined to
// the release platforms where guarded retirement has these kernel contracts.
func archiveInventoryCommand(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return nil
		}
		return err
	}
	return cmd, nil
}

func workerDirRetryPlatformSupported() error { return nil }

func workerDirRetryOwned(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid())
}

func workerDirRetryLinkCount(info os.FileInfo) uint64 {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(st.Nlink) //nolint:unconvert // uint16 on darwin, uint64 on linux
}

func sameWorkerDirRetryOwnerLinks(first, second os.FileInfo) bool {
	a, aOK := first.Sys().(*syscall.Stat_t)
	b, bOK := second.Sys().(*syscall.Stat_t)
	return aOK && bOK && a.Uid == b.Uid && a.Gid == b.Gid && a.Nlink == b.Nlink
}

func openWorkerDirRetryPlatformFile(root *os.Root, name string, flags int, perm os.FileMode) (*os.File, error) {
	flags |= syscall.O_NOFOLLOW
	if flags&os.O_CREATE == 0 {
		flags |= syscall.O_NONBLOCK
	}
	return root.OpenFile(name, flags, perm)
}
