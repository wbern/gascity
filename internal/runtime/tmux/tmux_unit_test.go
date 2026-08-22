package tmux

import (
	"context"
	"slices"
	"testing"
)

func TestTeardownNeverUsesDirectPIDSignalsAfterReplacement(t *testing.T) {
	// A same-token replacement (lstart has only second precision) and a
	// replacement after verification are both indistinguishable from the old
	// direct-signal path's perspective. Neither may trigger process discovery
	// or a direct signal; named tmux teardown is the only permitted action.
	for _, replacement := range []string{"same-token", "after-verification"} {
		t.Run(replacement, func(t *testing.T) {
			oldProbe := runProcessProbe
			runProcessProbe = func(context.Context, string, ...string) ([]byte, error) {
				t.Fatalf("direct PID teardown probe invoked after %s replacement", replacement)
				return nil, nil
			}
			t.Cleanup(func() { runProcessProbe = oldProbe })

			fe := &fakeExecutor{}
			tm := NewTmux()
			tm.exec = fe
			if err := tm.KillSessionWithProcessesExcluding("managed", []string{"101"}); err != nil {
				t.Fatalf("KillSessionWithProcessesExcluding: %v", err)
			}
			if want := [][]string{{"-u", "kill-session", "-t", "managed"}}; !slices.EqualFunc(fe.calls, want, slices.Equal) {
				t.Fatalf("tmux calls = %v, want named session teardown only %v", fe.calls, want)
			}
		})
	}
}

func TestKillPaneProcessesAreNoOpsUntilNamedRespawn(t *testing.T) {
	tm := NewTmux()
	fe := &fakeExecutor{}
	tm.exec = fe
	if err := tm.KillPaneProcesses("%9"); err != nil {
		t.Fatalf("KillPaneProcesses: %v", err)
	}
	if err := tm.KillPaneProcessesExcluding("%9", []string{"101"}); err != nil {
		t.Fatalf("KillPaneProcessesExcluding: %v", err)
	}
	if len(fe.calls) != 0 {
		t.Fatalf("tmux calls = %v, want no-op before RespawnPane(-k)", fe.calls)
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
