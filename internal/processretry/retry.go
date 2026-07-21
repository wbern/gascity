// Package processretry provides bounded retries for child-process launch
// failures that are known to occur before the child starts.
package processretry

import (
	"errors"
	"syscall"
	"time"
)

const (
	transientStartAttempts = 4
	transientStartBackoff  = 50 * time.Millisecond
)

// RunWithTransientStartRetry runs start and retries EAGAIN launch failures with
// a short bounded backoff. The caller must create a fresh child command on each
// invocation. It must only be used for process-start errors: retrying a child
// that may have run would risk duplicating a mutation.
func RunWithTransientStartRetry(start func() error) error {
	return runWithTransientStartRetry(start, time.Sleep)
}

func runWithTransientStartRetry(start func() error, sleep func(time.Duration)) error {
	for attempt := 0; attempt < transientStartAttempts; attempt++ {
		err := start()
		if err == nil || !errors.Is(err, syscall.EAGAIN) || attempt+1 == transientStartAttempts {
			return err
		}
		sleep(transientStartBackoff << attempt)
	}
	return nil
}
