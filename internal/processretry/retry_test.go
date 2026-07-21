package processretry

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

func TestRunWithTransientStartRetryRetriesEAGAINThenSucceeds(t *testing.T) {
	var calls int
	var delays []time.Duration
	err := runWithTransientStartRetry(func() error {
		calls++
		if calls < 3 {
			return syscall.EAGAIN
		}
		return nil
	}, func(delay time.Duration) {
		delays = append(delays, delay)
	})
	if err != nil {
		t.Fatalf("runWithTransientStartRetry() error = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	want := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("delays[%d] = %s, want %s", i, delays[i], want[i])
		}
	}
}

func TestRunWithTransientStartRetryStopsAfterBoundedAttempts(t *testing.T) {
	var calls int
	wantErr := syscall.EAGAIN
	err := runWithTransientStartRetry(func() error {
		calls++
		return wantErr
	}, func(time.Duration) {})
	if !errors.Is(err, wantErr) {
		t.Fatalf("runWithTransientStartRetry() error = %v, want %v", err, wantErr)
	}
	if calls != transientStartAttempts {
		t.Fatalf("calls = %d, want %d", calls, transientStartAttempts)
	}
}

func TestRunWithTransientStartRetryDoesNotRetryPermanentFailure(t *testing.T) {
	permanent := errors.New("permission denied")
	var calls int
	err := runWithTransientStartRetry(func() error {
		calls++
		return permanent
	}, func(time.Duration) {
		t.Fatal("sleep called for non-transient error")
	})
	if !errors.Is(err, permanent) {
		t.Fatalf("runWithTransientStartRetry() error = %v, want %v", err, permanent)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}
