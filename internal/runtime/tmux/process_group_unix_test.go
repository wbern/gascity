//go:build !windows

package tmux

import (
	"context"
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
