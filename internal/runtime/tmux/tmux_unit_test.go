package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
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
			if want := [][]string{{"-u", "show-environment", "-t", "managed", ownedScopeEnv}, {"-u", "kill-session", "-t", "managed"}}; !slices.EqualFunc(fe.calls, want, slices.Equal) {
				t.Fatalf("tmux calls = %v, want scope-record lookup then named session teardown %v", fe.calls, want)
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

func TestRespawnPaneWithWorkDirReplacesWindowWithoutRespawnPane(t *testing.T) {
	fe := &fakeExecutor{outs: []string{"@1\tbuild\t1\t/previous"}}
	tm := NewTmux()
	tm.exec = fe

	if err := tm.RespawnPaneWithWorkDir("managed", "/work", "agent --resume"); err != nil {
		t.Fatalf("RespawnPaneWithWorkDir: %v", err)
	}

	want := [][]string{
		{"-u", "display-message", "-p", "-t", "managed", "#{window_id}\t#{window_name}\t#{window_panes}\t#{pane_current_path}"},
		{"-u", "if-shell", "-F", "-t", "@1", "#{==:#{window_panes},1}", tmuxCommandLine([]string{"new-window", "-d", "-k", "-t", "@1", "-n", "build", "-c", "/work", tm.wrapReplacementCommand("agent --resume")}), "run-shell 'exit 77'"},
		{"-u", "show-environment", "-t", "managed", "GC_INSTANCE_TOKEN"},
	}
	if !slices.EqualFunc(fe.calls, want, slices.Equal) {
		t.Fatalf("tmux calls = %v, want atomic window replacement without respawn-pane %v", fe.calls, want)
	}
}

func TestRespawnPanePreservesCurrentWorkDir(t *testing.T) {
	fe := &fakeExecutor{outs: []string{"@2\tmain\t1\t/current"}}
	tm := NewTmux()
	tm.exec = fe

	if err := tm.RespawnPane("managed", "agent --resume"); err != nil {
		t.Fatalf("RespawnPane: %v", err)
	}
	if got := strings.Join(fe.calls[1], "\\x00"); !strings.Contains(got, "\\x00if-shell\\x00") || !strings.Contains(got, "'-c' '/current'") {
		t.Fatalf("replacement command = %v, want atomic current-directory replacement", fe.calls[1])
	}
}

func TestRespawnPaneRefusesWindowWithSiblingPanes(t *testing.T) {
	fe := &fakeExecutor{outs: []string{"@1\tmain\t2\t/work", ""}, errs: []error{nil, errors.New("exit 77")}}
	tm := NewTmux()
	tm.exec = fe

	err := tm.RespawnPane("managed", "agent --resume")
	if err == nil || !strings.Contains(err.Error(), "replacing window") {
		t.Fatalf("RespawnPane error = %v, want sibling-loss refusal", err)
	}
	if len(fe.calls) != 2 || !slices.Contains(fe.calls[1], "if-shell") {
		t.Fatalf("tmux calls = %v, want atomic predicate and no replacement", fe.calls)
	}
}

func TestRespawnPaneDoesNotRetireOriginalWindowWhenReplacementFails(t *testing.T) {
	fe := &fakeExecutor{outs: []string{"@1\tmain\t1\t/work", ""}, errs: []error{nil, errors.New("tmux failed")}}
	tm := NewTmux()
	tm.exec = fe

	err := tm.RespawnPane("managed", "agent --resume")
	if err == nil || !strings.Contains(err.Error(), "replacing window") {
		t.Fatalf("RespawnPane error = %v, want replacement failure", err)
	}
	if len(fe.calls) != 2 || !slices.Contains(fe.calls[1], "if-shell") {
		t.Fatalf("tmux calls = %v, want metadata lookup and failed replacement only", fe.calls)
	}
}

func TestWrapReplacementCommandPreservesShellOperators(t *testing.T) {
	tm := NewTmux()
	wrapped := tm.wrapReplacementCommand("false || printf recovered > result")
	if !strings.Contains(wrapped, "exec sh -c") || !strings.Contains(wrapped, "false || printf recovered > result") {
		t.Fatalf("replacement command = %q, want a quoted shell preserving operators", wrapped)
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
