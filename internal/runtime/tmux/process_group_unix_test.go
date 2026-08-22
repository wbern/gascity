//go:build !windows

package tmux

import (
	"context"
	"errors"
	"testing"
)

func TestSnapshotProcessTableProbeIsBoundedAndFailsClosed(t *testing.T) {
	called := false
	got := snapshotProcessTableWithProbe(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		called = true
		if name != "ps" || len(args) != 2 {
			t.Fatalf("probe = %s %v, want ps snapshot", name, args)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("snapshot probe has no deadline")
		}
		return nil, context.DeadlineExceeded
	})
	if !called || got != nil {
		t.Fatalf("snapshot = %v, called=%v; want fail-closed nil", got, called)
	}
}

func TestProcessStartTimeProbeIsBoundedAndFailsClosed(t *testing.T) {
	called := false
	got := processStartTimeWithProbe("101", func(ctx context.Context, name string, args ...string) ([]byte, error) {
		called = true
		if name != "ps" || len(args) != 4 {
			t.Fatalf("probe = %s %v, want ps start-time", name, args)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("start-time probe has no deadline")
		}
		return nil, errors.New("permission denied")
	})
	if !called || got != "" {
		t.Fatalf("start time = %q, called=%v; want fail-closed empty", got, called)
	}
}
