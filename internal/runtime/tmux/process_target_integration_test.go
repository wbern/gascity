//go:build integration

package tmux

import (
	"errors"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// processTarget is a process the integration tests started, identified by PID
// and start time so test cleanup never signals a recycled PID.
//
// This is test-only on purpose. Production code in this fork does not signal
// PIDs directly (see the no-direct-PID teardown, tracked as gcw-bbvcu); the
// tests that start their own tmux servers still need to be sure those servers
// are gone, and do it here.
type processTarget struct {
	PID       int
	StartTime string
}

func normalizeProcessStartTime(startTime string) string {
	return strings.Join(strings.Fields(startTime), " ")
}

// terminateProcesses kills each target whose current identity still matches,
// skipping empty targets and processes that are already gone.
func terminateProcesses(targets []processTarget) error {
	var errs []error
	for _, target := range targets {
		if target.PID <= 1 {
			continue
		}
		current, err := proctable.ProcessIdentity(target.PID)
		if err != nil {
			if !errors.Is(err, proctable.ErrProcessGone) {
				errs = append(errs, err)
			}
			continue
		}
		if normalizeProcessStartTime(current) != target.StartTime {
			continue // the PID now belongs to a different process
		}
		if err := syscall.Kill(target.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
