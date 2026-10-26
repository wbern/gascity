//go:build !unix

package git

import (
	"fmt"
	"os"
)

func openTrackedRegular(_ *os.Root, _ string) (*os.File, error) {
	return nil, fmt.Errorf("stable tracked-content reads unsupported on this platform")
}
