//go:build !windows

package tmux

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const processProbeTimeout = time.Second

var runProcessProbe = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// procIdentity is one process's parent, group, and start time captured in a
// single atomic ps snapshot. start is the identity token that survives PID
// reuse: when the kernel recycles a PID onto a new process, that process has a
// different start time, so a stale kill can be detected and skipped.
type procIdentity struct {
	ppid  string
	pgid  string
	start string
}

// snapshotProcessTable captures ppid, pgid, and start time for EVERY process in
// one ps call, keyed by PID. Descendant discovery then walks this in-memory
// snapshot instead of a slow live `pgrep -P` recursion (one exec per node,
// seconds under load). The live walk was the arming half of the session-massacre
// TOCTOU: during a stop/drain wave the agent trees collapse, the kernel recycles
// their PIDs onto unrelated processes inside the walk→kill window, and the kill
// loop then landed on whatever now owned each reused PID. A single atomic
// snapshot removes the multi-second discovery window; the teardown phase
// re-verifies identity before each signal. Returns nil on ps failure, which
// callers treat as "signal nothing" (safe: tmux kill-session still tears the
// pane down).
func snapshotProcessTable() map[string]procIdentity {
	return snapshotProcessTableWithProbe(runProcessProbe)
}

func snapshotProcessTableWithProbe(run func(context.Context, string, ...string) ([]byte, error)) map[string]procIdentity {
	ctx, cancel := context.WithTimeout(context.Background(), processProbeTimeout)
	defer cancel()
	return snapshotProcessTableWithContext(ctx, run)
}

func snapshotProcessTableWithContext(ctx context.Context, run func(context.Context, string, ...string) ([]byte, error)) map[string]procIdentity {
	// pid/ppid/pgid are single numeric tokens; lstart is the trailing
	// (space-containing) field, so parse the first three and join the rest.
	out, err := run(ctx, "ps", "-axo", "pid=,ppid=,pgid=,lstart=")
	if err != nil {
		return nil
	}
	return parseProcessTable(string(out))
}

// parseProcessTable decodes the portable ps shape used for teardown. Malformed
// records are deliberately ignored: discovery is fail-closed, so an incomplete
// snapshot may leave a process for tmux to reap but must never create a target.
func parseProcessTable(output string) map[string]procIdentity {
	table := make(map[string]procIdentity)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || !validProcessPID(fields[0]) {
			continue
		}
		table[fields[0]] = procIdentity{
			ppid:  fields[1],
			pgid:  fields[2],
			start: strings.Join(fields[3:], " "),
		}
	}
	return table
}

func processStartTimeWithProbe(pid string, run func(context.Context, string, ...string) ([]byte, error)) string {
	ctx, cancel := context.WithTimeout(context.Background(), processProbeTimeout)
	defer cancel()
	return processStartTimeWithContext(ctx, pid, run)
}

func processStartTimeWithContext(ctx context.Context, pid string, run func(context.Context, string, ...string) ([]byte, error)) string {
	out, err := run(ctx, "ps", "-o", "lstart=", "-p", pid)
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

// processStartTimeWithPhaseBudget gives each ps call at most one second while
// preserving the enclosing phase deadline. Once the phase expires, the probe
// is not launched and ownership fails closed.
func processStartTimeWithPhaseBudget(ctx context.Context, pid string) string {
	return processStartTimeWithPhaseBudgetWithProbe(ctx, pid, runProcessProbe)
}

func processStartTimeWithPhaseBudgetWithProbe(ctx context.Context, pid string, run func(context.Context, string, ...string) ([]byte, error)) string {
	if ctx.Err() != nil {
		return ""
	}
	probeCtx, cancel := context.WithTimeout(ctx, processProbeTimeout)
	defer cancel()
	return processStartTimeWithContext(probeCtx, pid, run)
}

// signalVerifiedProcess sends a direct Unix signal after the caller has
// identity-fenced the PID. It avoids a second external process between check
// and signal; invalid or already-gone PIDs are ignored.
func signalVerifiedProcess(pid, signal string) {
	n, err := strconv.Atoi(pid)
	if err != nil || n <= 1 {
		return
	}
	process, err := os.FindProcess(n)
	if err != nil {
		return
	}
	var sig syscall.Signal
	switch signal {
	case "TERM":
		sig = syscall.SIGTERM
	case "KILL":
		sig = syscall.SIGKILL
	default:
		return
	}
	_ = process.Signal(sig)
}
