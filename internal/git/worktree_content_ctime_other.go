//go:build !darwin && !linux

package git

import "os"

func trackedContentChangeStamp(_ os.FileInfo) string { return "" }
