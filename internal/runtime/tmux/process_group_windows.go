//go:build windows

package tmux

// procIdentity mirrors the unix type so cross-platform tmux.go compiles.
type procIdentity struct {
	ppid  string
	pgid  string
	start string
}

// snapshotProcessTable is a no-op on Windows: the POSIX PID-reuse kill race the
// unix implementation guards against does not apply here (Gas City does not run
// the signal-based teardown path on Windows). Returning nil makes callers signal
// nothing and fall back to tmux kill-session.
func snapshotProcessTable() map[string]procIdentity {
	return nil
}

// parseProcessTable mirrors the Unix parsing seam. Windows does not use the
// signal-based teardown path, so it intentionally produces no targets.
func parseProcessTable(_ string) map[string]procIdentity {
	return nil
}
