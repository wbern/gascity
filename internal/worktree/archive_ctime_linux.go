package worktree

import (
	"fmt"
	"os"
	"syscall"
)

func archiveChangeStamp(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Ctim.Sec, st.Ctim.Nsec)
}
