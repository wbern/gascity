package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestHookRunExportsItsDeadline(t *testing.T) {
	exe := buildTurnBoundEnvProbe(t)
	originalExecutable := hookRunExecutable
	t.Cleanup(func() { hookRunExecutable = originalExecutable })
	hookRunExecutable = func() (string, error) { return exe, nil }

	before := time.Now()
	var stdout, stderr bytes.Buffer
	if code := cmdHookRun([]string{"probe"}, hookRunOptions{Timeout: 10 * time.Second}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdHookRun = %d, want 0; stderr=%s", code, stderr.String())
	}
	m := regexp.MustCompile(`(?m)^` + hookDeadlineEnv + `=(.*)$`).FindStringSubmatch(stdout.String())
	if m == nil {
		t.Fatalf("gc hook run child env carries no %s", hookDeadlineEnv)
	}
	deadline, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(m[1]))
	if err != nil {
		t.Fatalf("parsing %s=%q: %v", hookDeadlineEnv, m[1], err)
	}
	if deadline.Before(before.Add(10*time.Second)) || deadline.After(time.Now().Add(10*time.Second)) {
		t.Fatalf("%s = %s, want now + 10s (run started %s)", hookDeadlineEnv, deadline, before)
	}
}

func TestNestedHookRunKeepsTheEarlierDeadline(t *testing.T) {
	exe := buildTurnBoundEnvProbe(t)
	originalExecutable := hookRunExecutable
	t.Cleanup(func() { hookRunExecutable = originalExecutable })
	hookRunExecutable = func() (string, error) { return exe, nil }
	deadlineLine := regexp.MustCompile(`(?m)^` + hookDeadlineEnv + `=(.*)$`)

	for _, tc := range []struct {
		name          string
		inheritedIn   time.Duration
		timeout       time.Duration
		wantInherited bool
	}{
		{name: "inherited earlier", inheritedIn: 2 * time.Second, timeout: 10 * time.Second, wantInherited: true},
		{name: "own earlier", inheritedIn: time.Minute, timeout: 10 * time.Second, wantInherited: false},
		// An inherited deadline already past cannot be the outer run's live
		// one; it is stale, and honoring it would skip every archive and
		// release every nudge.
		{name: "inherited already passed", inheritedIn: -time.Second, timeout: 10 * time.Second, wantInherited: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inherited := hookDeadlineEnvValue(time.Now().Add(tc.inheritedIn))
			t.Setenv(hookDeadlineEnv, inherited)
			before := time.Now()
			var stdout, stderr bytes.Buffer
			if code := cmdHookRun([]string{"probe"}, hookRunOptions{Timeout: tc.timeout}, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("cmdHookRun = %d; stderr=%s", code, stderr.String())
			}
			lines := deadlineLine.FindAllStringSubmatch(stdout.String(), -1)
			if len(lines) != 1 {
				t.Fatalf("child env carries %d %s entries, want 1", len(lines), hookDeadlineEnv)
			}
			got := strings.TrimSpace(lines[0][1])
			if tc.wantInherited {
				if got != inherited {
					t.Fatalf("%s = %s, want the inherited %s", hookDeadlineEnv, got, inherited)
				}
				return
			}
			deadline, err := time.Parse(time.RFC3339Nano, got)
			if err != nil {
				t.Fatalf("parsing %s=%q: %v", hookDeadlineEnv, got, err)
			}
			if deadline.Before(before.Add(tc.timeout)) || deadline.After(time.Now().Add(tc.timeout)) {
				t.Fatalf("%s = %s, want its own now + %s", hookDeadlineEnv, deadline, tc.timeout)
			}
		})
	}
}

func setHookProcessStart(t *testing.T, start time.Time) {
	t.Helper()
	prev := hookProcessStart
	hookProcessStart = start
	t.Cleanup(func() { hookProcessStart = prev })
}

func TestHookDeadlineLeavesArchiveTime(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	long := now.Add(-time.Minute)
	for _, tc := range []struct {
		name    string
		start   time.Time
		value   string
		want    bool
		wantErr bool
	}{
		{name: "no deadline", start: long, value: "", want: true},
		{name: "ample", start: long, value: hookDeadlineEnvValue(now.Add(time.Minute)), want: true},
		{name: "exactly the full margin", start: long, value: hookDeadlineEnvValue(now.Add(hookDeadlineArchiveMarginMax)), want: true},
		{name: "inside the full margin", start: long, value: hookDeadlineEnvValue(now.Add(hookDeadlineArchiveMarginMax - time.Millisecond)), want: false},
		{name: "default timeout, just started", start: now, value: hookDeadlineEnvValue(now.Add(defaultHookRunTimeout)), want: true},
		{name: "short timeout, fast hook", start: now.Add(-100 * time.Millisecond), value: hookDeadlineEnvValue(now.Add(800 * time.Millisecond)), want: true},
		{name: "short timeout, exactly a third left", start: now.Add(-600 * time.Millisecond), value: hookDeadlineEnvValue(now.Add(300 * time.Millisecond)), want: true},
		{name: "short timeout, under a third left", start: now.Add(-700 * time.Millisecond), value: hookDeadlineEnvValue(now.Add(200 * time.Millisecond)), want: false},
		{name: "passed", start: long, value: hookDeadlineEnvValue(now.Add(-time.Second)), want: false},
		{name: "malformed", start: long, value: "soon", want: false, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(hookDeadlineEnv, tc.value)
			got, err := hookDeadlineLeavesArchiveTime(now, tc.start)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("hookDeadlineLeavesArchiveTime = (%v, %v), want (%v, error=%v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestHookDeadlineNudgeAckFailsClosed(t *testing.T) {
	now := time.Now()
	setHookProcessStart(t, now.Add(-time.Minute))
	for _, tc := range []struct {
		name, value string
		want        bool
	}{
		{"outside hook", "", true},
		{"time remains", hookDeadlineEnvValue(now.Add(time.Hour)), true},
		{"deadline passed", hookDeadlineEnvValue(now.Add(-time.Second)), false},
		{"inside reserve", hookDeadlineEnvValue(now.Add(time.Second)), false},
		{"malformed", "soon", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(hookDeadlineEnv, tc.value)
			var warning bytes.Buffer
			if got := hookDeadlineAllowsNudgeAck("batch", &warning); got != tc.want {
				t.Fatalf("ack allowed=%v, want %v", got, tc.want)
			}
			if (warning.Len() == 0) != tc.want {
				t.Fatalf("warning=%q, allowed=%v", warning.String(), tc.want)
			}
			if !tc.want {
				if end, ok := hookWorkEnd(); !ok || end.After(time.Now()) {
					t.Fatalf("unsafe deadline left work time: %s, %v", end, ok)
				}
			}
		})
	}
}
