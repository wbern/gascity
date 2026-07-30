package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// loadAvgTimeout bounds the portable load probe. It runs once per reap pass on a
// controller tick, so a hung sysctl must not stall it.
const loadAvgTimeout = 2 * time.Second

// loadAvgSysctlFn reads the darwin/BSD load average. Indirected through a var so
// the parser is testable without a process.
var loadAvgSysctlFn = func() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), loadAvgTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "sysctl", "-n", "vm.loadavg").Output()
}

// oneMinuteLoadAverage returns the host's 1-minute load average.
//
// Go has no stdlib load average, and the two platforms expose it differently:
// Linux as the first field of /proc/loadavg ("0.52 0.58 0.59 1/1234 5678"),
// darwin and the BSDs as sysctl vm.loadavg ("{ 10.93 35.71 43.73 }"). /proc is
// tried first and sysctl is the fallback, matching how the other process probes
// in this tree are layered.
//
// An error means "could not read the load", and callers must treat that as
// "proceed" rather than "throttle" — see the note on the reaper's load guard.
func oneMinuteLoadAverage() (float64, error) {
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		return parseFirstLoadField(string(data))
	}
	out, err := loadAvgSysctlFn()
	if err != nil {
		return 0, fmt.Errorf("reading load average: %w", err)
	}
	return parseFirstLoadField(strings.NewReplacer("{", " ", "}", " ").Replace(string(out)))
}

// parseFirstLoadField takes the first whitespace-separated float in s, which is
// the 1-minute figure in both platforms' formats once braces are stripped.
func parseFirstLoadField(s string) (float64, error) {
	for _, field := range strings.Fields(s) {
		v, err := strconv.ParseFloat(field, 64)
		if err != nil {
			continue
		}
		if v < 0 {
			return 0, fmt.Errorf("negative load average %q", field)
		}
		return v, nil
	}
	return 0, fmt.Errorf("no load average field in %q", strings.TrimSpace(s))
}
