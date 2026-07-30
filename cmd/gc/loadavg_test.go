package main

import (
	"errors"
	"runtime"
	"testing"
)

func TestParseFirstLoadField_LinuxFormat(t *testing.T) {
	got, err := parseFirstLoadField("0.52 0.58 0.59 1/1234 5678")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != 0.52 {
		t.Errorf("got %v, want 0.52", got)
	}
}

func TestParseFirstLoadField_DarwinFormat(t *testing.T) {
	// sysctl -n vm.loadavg renders braces around the three figures.
	got, err := parseFirstLoadField(" 10.93 35.71 43.73 ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != 10.93 {
		t.Errorf("got %v, want 10.93", got)
	}
}

func TestParseFirstLoadField_RejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "   ", "not-a-number", "{ }"} {
		if _, err := parseFirstLoadField(in); err == nil {
			t.Errorf("parseFirstLoadField(%q) = nil error, want a failure", in)
		}
	}
}

func TestParseFirstLoadField_RejectsNegative(t *testing.T) {
	if _, err := parseFirstLoadField("-1.0 0.5 0.5"); err == nil {
		t.Error("negative load accepted; a negative load is not a real reading")
	}
}

// TestOneMinuteLoadAverage_ReadsOnThisHost is the regression test for the whole
// point: the probe must work on the host it runs on. Linux exercises /proc,
// darwin exercises the sysctl fallback.
func TestOneMinuteLoadAverage_ReadsOnThisHost(t *testing.T) {
	got, err := oneMinuteLoadAverage()
	if err != nil {
		t.Fatalf("oneMinuteLoadAverage on %s: %v", runtime.GOOS, err)
	}
	if got < 0 {
		t.Errorf("load = %v on %s, want a non-negative reading", got, runtime.GOOS)
	}
}

// TestOneMinuteLoadAverage_ErrorsWhenUnavailable pins that an unreadable load is
// an ERROR rather than a zero. Zero would read as "idle host", which is the
// opposite of the truth and would make the throttle permissive at exactly the
// wrong moment.
func TestOneMinuteLoadAverage_ErrorsWhenUnavailable(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("/proc/loadavg answers directly on linux, so the sysctl stub is unreachable")
	}
	prev := loadAvgSysctlFn
	loadAvgSysctlFn = func() ([]byte, error) { return nil, errors.New("sysctl exploded") }
	t.Cleanup(func() { loadAvgSysctlFn = prev })

	if _, err := oneMinuteLoadAverage(); err == nil {
		t.Error("oneMinuteLoadAverage returned nil error with no readable source; callers cannot tell 0 from unknown")
	}
}
