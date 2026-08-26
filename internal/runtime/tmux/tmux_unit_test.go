package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestSelectBindingLine(t *testing.T) {
	const table = "bind-key -T prefix C-g display-popup\nbind-key -T prefix n next-window\nbind-key -r -T prefix Up resize-pane -U\nbind-key malformed"
	for _, tt := range []struct{ name, key, want string }{
		{"exact", "n", "bind-key -T prefix n next-window"},
		{"near", "g", ""},
		{"repeat", "Up", "bind-key -r -T prefix Up resize-pane -U"},
		{"absent", "z", ""},
		{"malformed", "malformed", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectBindingLine(table, tt.key); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

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
	fe := &fakeExecutor{outs: []string{"@1\tbuild\t1\t/previous\tmanaged"}}
	tm := NewTmux()
	tm.exec = fe

	if err := tm.RespawnPaneWithWorkDir("managed", "/work", "agent --resume"); err != nil {
		t.Fatalf("RespawnPaneWithWorkDir: %v", err)
	}

	want := [][]string{
		{"-u", "display-message", "-p", "-t", "managed", "#{window_id}\t#{window_name}\t#{window_panes}\t#{pane_current_path}\t#{session_name}"},
		{"-u", "show-environment", "-t", "managed", ownedScopeEnv},
		{"-u", "if-shell", "-F", "-t", "@1", "#{==:#{window_panes},1}", tmuxCommandLine([]string{"new-window", "-d", "-k", "-t", "@1", "-n", "build", "-c", "/work", tm.wrapReplacementCommand("managed", "agent --resume")}), "run-shell 'exit 77'"},
	}
	if !slices.EqualFunc(fe.calls, want, slices.Equal) {
		t.Fatalf("tmux calls = %v, want atomic window replacement without respawn-pane %v", fe.calls, want)
	}
}

func TestRespawnPaneRetiresRecordedScopeBeforeReplacement(t *testing.T) {
	t.Setenv(AgentSliceEnv, "gascity-agents.slice")
	fs := &fakeOwnedScopes{}
	fe := &fakeExecutor{outs: []string{
		"@1\tbuild\t1\t/previous\tmanaged",
		ownedScopeEnv + "=gascity-pane-0123456789abcdef0123456789abcdef.scope",
		ownedScopeInvocationEnv + "=old-invocation",
		ownedScopeTokenEnv + "=instance-token",
		"GC_INSTANCE_TOKEN=instance-token",
		"",
		"%9",
		"GC_INSTANCE_TOKEN=instance-token",
		"", "", "",
	}, rejectPaneSessionTargets: true}
	tm := NewTmux()
	tm.exec = fe
	tm.ownedScopes = fs
	tm.agentSlice.probe = func(string) error { return nil }

	if err := tm.RespawnPaneWithWorkDir("%0", "/work", "agent --resume"); err != nil {
		t.Fatalf("RespawnPaneWithWorkDir: %v", err)
	}
	if got, want := fs.stopped, (ownedScope{unit: "gascity-pane-0123456789abcdef0123456789abcdef.scope", invocationID: "old-invocation"}); got != want {
		t.Fatalf("stopped scope = %+v, want old recorded scope %+v", got, want)
	}
	if len(fe.calls) < 6 || !slices.Contains(fe.calls[5], "if-shell") {
		t.Fatalf("tmux calls = %v, want old scope verification before replacement", fe.calls)
	}
	if got := fe.calls[6]; !slices.Contains(got, "managed:^.0") {
		t.Fatalf("replacement identity lookup = %v, want stable session identity", got)
	}
	for _, call := range fe.calls[7:] {
		if !slices.Contains(call, "show-environment") && !slices.Contains(call, "set-environment") {
			continue
		}
		for i, arg := range call[:len(call)-1] {
			if arg == "-t" && strings.HasPrefix(call[i+1], "%") {
				t.Fatalf("session environment call = %v, want stable session target", call)
			}
		}
	}
}

func TestRespawnPaneCleansReplacementScopeWhenPaneIdentityLookupFails(t *testing.T) {
	t.Setenv(AgentSliceEnv, "gascity-agents.slice")
	fs := &fakeOwnedScopes{}
	fe := &fakeExecutor{
		outs: []string{
			"@1\tbuild\t1\t/previous\tmanaged",
			ownedScopeEnv + "=gascity-pane-0123456789abcdef0123456789abcdef.scope",
			ownedScopeInvocationEnv + "=old-invocation",
			ownedScopeTokenEnv + "=instance-token",
			"GC_INSTANCE_TOKEN=instance-token",
			"",
		},
		errs: []error{nil, nil, nil, nil, nil, nil, errors.New("replacement pane unavailable")},
	}
	tm := NewTmux()
	tm.exec = fe
	tm.ownedScopes = fs
	tm.agentSlice.probe = func(string) error { return nil }

	err := tm.RespawnPaneWithWorkDir("managed", "/work", "agent --resume")
	if err == nil || !strings.Contains(err.Error(), "resolving replacement pane identity") {
		t.Fatalf("RespawnPaneWithWorkDir error = %v, want replacement pane identity failure", err)
	}
	if got := len(fs.stops); got != 2 {
		t.Fatalf("stopped scopes = %+v, want old and replacement scopes stopped", fs.stops)
	}
	if got := fs.stops[1]; got.unit == "" || got.unit == fs.stops[0].unit {
		t.Fatalf("replacement scope = %+v, want a distinct witnessed replacement scope", got)
	}
	if got := fe.calls[len(fe.calls)-1]; !slices.Equal(got, []string{"-u", "kill-window", "-t", "@1"}) {
		t.Fatalf("last tmux call = %v, want replacement window cleanup", got)
	}
}

func TestRespawnPaneCleansReplacementScopeWhenRecordFails(t *testing.T) {
	t.Setenv(AgentSliceEnv, "gascity-agents.slice")
	fs := &fakeOwnedScopes{}
	fe := &fakeExecutor{
		outs: []string{
			"@1\tbuild\t1\t/previous\tmanaged",
			ownedScopeEnv + "=gascity-pane-0123456789abcdef0123456789abcdef.scope",
			ownedScopeInvocationEnv + "=old-invocation",
			ownedScopeTokenEnv + "=instance-token",
			"GC_INSTANCE_TOKEN=instance-token",
			"",
			"%9",
			"GC_INSTANCE_TOKEN=instance-token",
		},
		errs: []error{nil, nil, nil, nil, nil, nil, nil, nil, errors.New("record unavailable")},
	}
	tm := NewTmux()
	tm.exec = fe
	tm.ownedScopes = fs
	tm.agentSlice.probe = func(string) error { return nil }

	err := tm.RespawnPaneWithWorkDir("managed", "/work", "agent --resume")
	if err == nil || !strings.Contains(err.Error(), "recording replacement owned scope") {
		t.Fatalf("RespawnPaneWithWorkDir error = %v, want replacement record failure", err)
	}
	if got := len(fs.stops); got != 2 {
		t.Fatalf("stopped scopes = %+v, want old and replacement scopes stopped", fs.stops)
	}
	if got := fe.calls[len(fe.calls)-1]; !slices.Equal(got, []string{"-u", "kill-window", "-t", "@1"}) {
		t.Fatalf("last tmux call = %v, want replacement window cleanup", got)
	}
}

func TestRespawnPanePreservesCurrentWorkDir(t *testing.T) {
	fe := &fakeExecutor{outs: []string{"@2\tmain\t1\t/current\tmanaged"}}
	tm := NewTmux()
	tm.exec = fe

	if err := tm.RespawnPane("managed", "agent --resume"); err != nil {
		t.Fatalf("RespawnPane: %v", err)
	}
	if got := strings.Join(fe.calls[2], "\\x00"); !strings.Contains(got, "\\x00if-shell\\x00") || !strings.Contains(got, "'-c' '/current'") {
		t.Fatalf("replacement command = %v, want atomic current-directory replacement", fe.calls[2])
	}
}

func TestRespawnPaneRefusesWindowWithSiblingPanes(t *testing.T) {
	fe := &fakeExecutor{outs: []string{"@1\tmain\t2\t/work\tmanaged", "", ""}, errs: []error{nil, nil, errors.New("exit 77")}}
	tm := NewTmux()
	tm.exec = fe

	err := tm.RespawnPane("managed", "agent --resume")
	if err == nil || !strings.Contains(err.Error(), "window has 2 panes") {
		t.Fatalf("RespawnPane error = %v, want sibling-loss refusal", err)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("tmux calls = %v, want metadata eligibility check and no replacement", fe.calls)
	}
}

func TestRespawnPaneRefusesMultiPaneWindowBeforeRetiringOwnedScope(t *testing.T) {
	fs := &fakeOwnedScopes{}
	fe := &fakeExecutor{outs: []string{"@1\tmain\t2\t/work\tmanaged"}}
	tm := NewTmux()
	tm.exec = fe
	tm.ownedScopes = fs

	err := tm.RespawnPane("managed", "agent --resume")
	if err == nil || !strings.Contains(err.Error(), "window has 2 panes") {
		t.Fatalf("RespawnPane error = %v, want multi-pane refusal", err)
	}
	if fs.stopped != (ownedScope{}) {
		t.Fatalf("stopped scope = %+v, want original scope preserved", fs.stopped)
	}
	if want := 1; len(fe.calls) != want {
		t.Fatalf("tmux calls = %v, want only metadata eligibility check", fe.calls)
	}
}

func TestRespawnPaneDoesNotRetireOriginalWindowWhenReplacementFails(t *testing.T) {
	fe := &fakeExecutor{outs: []string{"@1\tmain\t1\t/work\tmanaged", "", ""}, errs: []error{nil, nil, errors.New("tmux failed")}}
	tm := NewTmux()
	tm.exec = fe

	err := tm.RespawnPane("managed", "agent --resume")
	if err == nil || !strings.Contains(err.Error(), "replacing window") {
		t.Fatalf("RespawnPane error = %v, want replacement failure", err)
	}
	if len(fe.calls) != 3 || !slices.Contains(fe.calls[2], "if-shell") {
		t.Fatalf("tmux calls = %v, want metadata lookup and failed replacement only", fe.calls)
	}
}

func TestWrapReplacementCommandPreservesShellOperators(t *testing.T) {
	tm := NewTmux()
	wrapped := tm.wrapReplacementCommand("managed", "false || printf recovered > result")
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
