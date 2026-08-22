package tmux

import (
	"slices"
	"testing"
	"time"
)

func TestBuildKillTargetsFromSnapshotOrdersDescendantsAndFencesForeignGroupMembers(t *testing.T) {
	snapshot := map[string]procIdentity{
		"100": {ppid: "9", pgid: "100", start: "root"},
		"200": {ppid: "100", pgid: "100", start: "child"},
		"300": {ppid: "200", pgid: "100", start: "grandchild"},
		"400": {ppid: "1", pgid: "100", start: "orphan"},
		"450": {ppid: "900", pgid: "100", start: "subreaper orphan"},
		"500": {ppid: "1", pgid: "999", start: "foreign"},
	}

	descendants, reparented, identities := buildKillTargetsFromSnapshot("100", snapshot)
	if want := []string{"300", "200"}; !slices.Equal(descendants, want) {
		t.Fatalf("descendants = %v, want deepest-first %v", descendants, want)
	}
	if want := []string{"400", "450"}; !slices.Equal(reparented, want) {
		t.Fatalf("reparented = %v, want %v", reparented, want)
	}
	if _, found := identities["500"]; found {
		t.Fatal("foreign PGID member must never become a kill target")
	}
}

func TestBuildKillTargetsFromSnapshotRejectsMissingOrSystemRoot(t *testing.T) {
	snapshot := map[string]procIdentity{
		"100": {ppid: "9", pgid: "100", start: "root"},
		"200": {ppid: "404", pgid: "100", start: "would-be child"},
	}
	for _, root := range []string{"404", "1", "0", "bad"} {
		descendants, reparented, identities := buildKillTargetsFromSnapshot(root, snapshot)
		if len(descendants) != 0 || len(reparented) != 0 || len(identities) != 0 {
			t.Fatalf("root %q produced targets descendants=%v reparented=%v identity=%v", root, descendants, reparented, identities)
		}
	}
}

func TestBuildKillTargetsFromSnapshotRejectsIdentitylessRoot(t *testing.T) {
	snapshot := map[string]procIdentity{
		"100": {ppid: "9", pgid: "100"},
		"200": {ppid: "100", pgid: "100", start: "child"},
	}
	descendants, reparented, identities := buildKillTargetsFromSnapshot("100", snapshot)
	if len(descendants) != 0 || len(reparented) != 0 || len(identities) != 0 {
		t.Fatalf("identity-less root produced targets descendants=%v reparented=%v identity=%v", descendants, reparented, identities)
	}
}

func TestParseProcessTableSkipsMalformedAndSpecialPIDs(t *testing.T) {
	snapshot := parseProcessTable("  0 1 0 bad\n  1 1 1 bad\n bad 1 2 bad\n  101 100 101 Mon Jul 6 08:00:00 2026\n  102 101 101\n")
	if got := snapshot["101"]; got != (procIdentity{ppid: "100", pgid: "101", start: "Mon Jul 6 08:00:00 2026"}) {
		t.Fatalf("snapshot[101] = %+v", got)
	}
	if len(snapshot) != 1 {
		t.Fatalf("snapshot contains malformed or special PIDs: %+v", snapshot)
	}
}

func TestBuildKillTargetsFromSnapshotTerminatesCycles(t *testing.T) {
	snapshot := map[string]procIdentity{
		"100": {ppid: "200", pgid: "100", start: "root"},
		"200": {ppid: "100", pgid: "100", start: "child"},
	}

	descendants, _, _ := buildKillTargetsFromSnapshot("100", snapshot)
	if want := []string{"200"}; !slices.Equal(descendants, want) {
		t.Fatalf("descendants = %v, want cycle-safe %v", descendants, want)
	}
}

func TestKillIdentityMatches(t *testing.T) {
	for _, tt := range []struct {
		name, current, want string
		match               bool
	}{
		{"exact", "start-a", "start-a", true},
		{"recycled", "start-b", "start-a", false},
		{"gone", "", "start-a", false},
		{"absent snapshot", "start-a", "", false},
		{"both empty", "", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := killIdentityMatches(tt.current, tt.want); got != tt.match {
				t.Fatalf("killIdentityMatches(%q, %q) = %v, want %v", tt.current, tt.want, got, tt.match)
			}
		})
	}
}

func TestTerminateVerifiedProcessSetExitsAfterTERM(t *testing.T) {
	current := "start-a"
	var signals []string
	now := time.Unix(0, 0)
	terminateVerifiedProcessSet(
		[]string{"101"}, map[string]string{"101": "start-a"}, time.Second,
		func(string) string { return current },
		func(pid, signal string) { signals = append(signals, signal+":"+pid); current = "" },
		func(time.Duration) { t.Fatal("must exit before sleeping after TERM") },
		func() time.Time { return now },
	)
	if want := []string{"TERM:101"}; !slices.Equal(signals, want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
}

func TestTerminateVerifiedProcessSetSkipsReusedPIDBeforeKILL(t *testing.T) {
	current := "start-a"
	var signals []string
	now := time.Unix(0, 0)
	terminateVerifiedProcessSet(
		[]string{"101"}, map[string]string{"101": "start-a"}, time.Second,
		func(string) string { return current },
		func(pid, signal string) {
			signals = append(signals, signal+":"+pid)
			if signal == "TERM" {
				current = "reused-start"
			}
		},
		func(time.Duration) { now = now.Add(processExitCheckInterval) },
		func() time.Time { return now },
	)
	if want := []string{"TERM:101"}; !slices.Equal(signals, want) {
		t.Fatalf("signals = %v, want PID-reuse guard to suppress KILL", signals)
	}
}

func TestTerminateVerifiedProcessSetRefusesUnreadableAndAbsentSnapshotPIDs(t *testing.T) {
	var signals []string
	now := time.Unix(0, 0)
	terminateVerifiedProcessSet(
		[]string{"101", "102"}, map[string]string{"101": "start-a"}, time.Second,
		func(string) string { return "" },
		func(pid, signal string) { signals = append(signals, signal+":"+pid) },
		func(time.Duration) { t.Fatal("unowned PIDs must not enter the grace loop") },
		func() time.Time { return now },
	)
	if len(signals) != 0 {
		t.Fatalf("signals = %v, want none", signals)
	}
}

func TestTerminateVerifiedProcessSetForRootRefusesRecycledRoot(t *testing.T) {
	var signals []string
	now := time.Unix(0, 0)
	terminateVerifiedProcessSetForRoot(
		[]string{"101"}, "100", map[string]string{"100": "root-start", "101": "child-start"}, time.Second,
		func(pid string) string {
			if pid == "100" {
				return "reused-root"
			}
			return "child-start"
		},
		func(pid, signal string) { signals = append(signals, signal+":"+pid) },
		func(time.Duration) { t.Fatal("recycled root must not enter grace loop") },
		func() time.Time { return now },
	)
	if len(signals) != 0 {
		t.Fatalf("signals = %v, want none after root reuse", signals)
	}
}

func TestProviderEnvSkipsEscapeForPiAlias(t *testing.T) {
	if !providerEnvSkipsEscape("my-pi/tmux") {
		t.Fatal("pi provider alias should skip pre-enter Escape")
	}
}

func TestProviderEnvSkipsEscapeForCopilot(t *testing.T) {
	if !providerEnvSkipsEscape("copilot") {
		t.Fatal("copilot provider should skip pre-enter Escape")
	}
}

// TestComputeExcludingKillSet_SelfCloseExcludesCallerKeepsAgent locks in the
// fix for the self-close wedge: when `gc session close` runs from inside the
// pane it is tearing down, the caller is a descendant of the pane leader (the
// agent). The caller must be excluded from the TERM list so it survives long
// enough to finish cleanup, while the pane leader (agent) is still reached.
func TestComputeExcludingKillSet_SelfCloseExcludesCallerKeepsAgent(t *testing.T) {
	const (
		agentPID  = "100" // pane leader (e.g. the coding agent) — must be killed
		shellPID  = "101" // intermediate shell spawned by the agent
		callerPID = "102" // gc session close — the excluded caller
	)
	exclude := map[string]bool{callerPID: true}

	killList, killPaneLeader := computeExcludingKillSet(
		agentPID,
		[]string{shellPID, callerPID},
		nil,
		exclude,
	)

	if !killPaneLeader {
		t.Error("pane leader (agent) must be killed, but it was reported excluded")
	}
	if slices.Contains(killList, callerPID) {
		t.Errorf("caller %s must be excluded from TERM list, got %v", callerPID, killList)
	}
	if !slices.Contains(killList, shellPID) {
		t.Errorf("non-excluded descendant %s must be in TERM list, got %v", shellPID, killList)
	}
}

// TestComputeExcludingKillSet_ExternalCallerKillsEverything verifies that when
// the caller lives outside the pane (e.g. the supervisor running the close),
// excluding its PID is a harmless no-op: every process in the pane's tree is
// still terminated.
func TestComputeExcludingKillSet_ExternalCallerKillsEverything(t *testing.T) {
	const agentPID = "200"
	exclude := map[string]bool{"999": true} // external caller, not in the pane tree

	killList, killPaneLeader := computeExcludingKillSet(
		agentPID,
		[]string{"201"},
		[]string{"202"},
		exclude,
	)

	if !killPaneLeader {
		t.Error("pane leader must be killed for an external caller")
	}
	if !slices.Contains(killList, "201") || !slices.Contains(killList, "202") {
		t.Errorf("all pane descendants must be killed, got %v", killList)
	}
}

// TestComputeExcludingKillSet_ExcludedPaneLeaderSurvives guards the degenerate
// case where the pane leader itself is in the exclusion set: it must not be
// signaled directly (the final tmux kill-session reaps it instead).
func TestComputeExcludingKillSet_ExcludedPaneLeaderSurvives(t *testing.T) {
	const agentPID = "300"
	exclude := map[string]bool{agentPID: true}

	_, killPaneLeader := computeExcludingKillSet(agentPID, nil, nil, exclude)

	if killPaneLeader {
		t.Error("an excluded pane leader must not be killed directly")
	}
}

func TestTerminateProcessSetReturnsWhenTerminatedProcessesExit(t *testing.T) {
	alive := map[string]bool{"101": true, "102": true}
	var signals []string
	var sleeps []time.Duration
	now := time.Unix(0, 0)

	terminateProcessSet(
		[]string{"101", "102"},
		time.Second,
		func(pid, signal string) {
			signals = append(signals, signal+":"+pid)
			if signal == "TERM" {
				alive[pid] = false
			}
		},
		func(pid string) bool { return alive[pid] },
		func(delay time.Duration) {
			sleeps = append(sleeps, delay)
			now = now.Add(delay)
		},
		func() time.Time { return now },
	)

	if want := []string{"TERM:101", "TERM:102"}; !slices.Equal(signals, want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
	if len(sleeps) != 0 {
		t.Fatalf("sleep calls = %v, want none after TERM made every process exit", sleeps)
	}
}

func TestTerminateProcessSetKillsOnlyProcessesStillAliveAfterGracePeriod(t *testing.T) {
	alive := map[string]bool{"201": true, "202": true}
	var signals []string
	var slept time.Duration
	now := time.Unix(0, 0)

	terminateProcessSet(
		[]string{"201", "202"},
		2*processExitCheckInterval,
		func(pid, signal string) {
			signals = append(signals, signal+":"+pid)
			if signal == "TERM" && pid == "201" {
				alive[pid] = false
			}
		},
		func(pid string) bool { return alive[pid] },
		func(delay time.Duration) {
			slept += delay
			now = now.Add(delay)
		},
		func() time.Time { return now },
	)

	want := []string{"TERM:201", "TERM:202", "KILL:202"}
	if !slices.Equal(signals, want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
	if slept != 2*processExitCheckInterval {
		t.Fatalf("slept = %s, want full grace period %s for surviving process", slept, 2*processExitCheckInterval)
	}
}

func TestTerminateProcessSetReturnsWhenProcessExitsDuringGracePeriod(t *testing.T) {
	var signals []string
	checks := 0
	slept := time.Duration(0)
	now := time.Unix(0, 0)

	terminateProcessSet(
		[]string{"301"},
		time.Second,
		func(pid, signal string) { signals = append(signals, signal+":"+pid) },
		func(string) bool {
			checks++
			return checks < 3
		},
		func(delay time.Duration) {
			slept += delay
			now = now.Add(delay)
		},
		func() time.Time { return now },
	)

	if want := []string{"TERM:301"}; !slices.Equal(signals, want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
	if slept != 2*processExitCheckInterval {
		t.Fatalf("slept = %s, want two observations (%s)", slept, 2*processExitCheckInterval)
	}
}

func TestTerminateProcessSetCountsProbeTimeAgainstGracePeriod(t *testing.T) {
	var signals []string
	slept := time.Duration(0)
	now := time.Unix(0, 0)
	probeDuration := 2 * processExitCheckInterval

	terminateProcessSet(
		[]string{"401"},
		3*processExitCheckInterval,
		func(pid, signal string) { signals = append(signals, signal+":"+pid) },
		func(string) bool {
			now = now.Add(probeDuration)
			return true
		},
		func(delay time.Duration) {
			slept += delay
			now = now.Add(delay)
		},
		func() time.Time { return now },
	)

	if want := []string{"TERM:401", "KILL:401"}; !slices.Equal(signals, want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
	if slept != processExitCheckInterval {
		t.Fatalf("slept = %s, want remaining grace budget %s after slow probe", slept, processExitCheckInterval)
	}
}
