package git

import (
	"fmt"
	"os"
	"syscall"
)

func trackedContentChangeStamp(info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", stat.Ctim.Sec, stat.Ctim.Nsec)
}
