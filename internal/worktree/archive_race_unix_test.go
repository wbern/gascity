//go:build unix

package worktree

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestArchiveReadRejectsRacedSpecialReplacement(t *testing.T) {
	for _, kind := range []string{"fifo", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(base, "note.md")
			if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := archiveOpenRoot(base)
			if err != nil {
				t.Fatal(err)
			}
			defer archiveClose(root)
			_, _, err = archiveReadAfterProbe(root, "note.md", 1024, func() {
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if kind == "fifo" {
					if err := syscall.Mkfifo(path, 0o600); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Symlink("note.md.old", path); err != nil {
						t.Fatal(err)
					}
				}
			})
			if err == nil {
				t.Fatal("raced special replacement accepted")
			}
		})
	}
}
