//go:build unix

package worktree

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Nonblocking prevents a raced FIFO replacement from hanging before f.Stat;
// nofollow prevents reading a replacement symlink even inside the root.
func archiveOpenRegular(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

func archiveRetainOriginal(root *os.Root, path, sourcePath, target string, expected os.FileInfo) error {
	before, err := root.Lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || !archiveSame(before, expected) {
		return errors.New("original inode changed before retention")
	}
	// os.Root.Link only supports destinations within one root. Cross-root
	// retention uses the validated namespace and checks both identities after
	// linking; a raced link never authorizes successful publication.
	if err := os.Link(sourcePath, target); err != nil {
		return err
	}
	linked, err := os.Lstat(target)
	if err != nil {
		return err
	}
	after, err := root.Lstat(path)
	if err != nil {
		return err
	}
	if !linked.Mode().IsRegular() || !archiveSameContentMetadata(expected, linked) || !archiveSameContentMetadata(expected, after) {
		return errors.New("retained inode identity mismatch")
	}
	return nil
}

func archiveOwned(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint32(os.Geteuid()) == st.Uid
}

func archiveInodeIdentity(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}
