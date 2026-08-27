package tmux

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestSystemdUnitGone(t *testing.T) {
	if !systemdUnitGone(errors.New("systemctl show: Unit tmux-spawn-1.scope not found")) {
		t.Fatal("already-gone systemd scope was not treated as idempotent")
	}
	if systemdUnitGone(errors.New("permission denied")) {
		t.Fatal("command failure must retain the scope for diagnostics")
	}
}

func TestSystemdUserScopesStopFailsClosedOnReusedInvocation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd user scopes require Linux")
	}
	oldShow, oldStop := systemdShowProperty, systemdStopUnit
	t.Cleanup(func() {
		systemdShowProperty, systemdStopUnit = oldShow, oldStop
	})
	systemdShowProperty = func(string, string) (string, error) { return "replacement", nil }
	systemdStopUnit = func(string) error {
		t.Fatal("stop called for a scope whose invocation was reused")
		return nil
	}

	err := (systemdUserScopes{}).stop(ownedScope{unit: "gascity-pane-0123456789abcdef0123456789abcdef.scope", invocationID: "original"})
	if err == nil || !strings.Contains(err.Error(), "invocation changed") {
		t.Fatalf("stop error = %v, want invocation mismatch", err)
	}
}

func TestSystemdUserScopesStopTreatsGoneUnitAsIdempotent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd user scopes require Linux")
	}
	oldShow, oldStop := systemdShowProperty, systemdStopUnit
	t.Cleanup(func() {
		systemdShowProperty, systemdStopUnit = oldShow, oldStop
	})
	systemdShowProperty = func(string, string) (string, error) {
		return "", errors.New("Unit gascity-pane-0123456789abcdef0123456789abcdef.scope not found")
	}
	systemdStopUnit = func(string) error {
		t.Fatal("stop called for an already-gone scope")
		return nil
	}

	if err := (systemdUserScopes{}).stop(ownedScope{unit: "gascity-pane-0123456789abcdef0123456789abcdef.scope", invocationID: "original"}); err != nil {
		t.Fatalf("stop already-gone scope: %v", err)
	}
}

func TestSystemdUserScopesStopAcceptsRetainedInactiveOrFailedUnit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd user scopes require Linux")
	}
	for _, state := range []string{"inactive", "failed"} {
		t.Run(state, func(t *testing.T) {
			oldShow, oldStop := systemdShowProperty, systemdStopUnit
			t.Cleanup(func() {
				systemdShowProperty, systemdStopUnit = oldShow, oldStop
			})
			showCalls := 0
			systemdShowProperty = func(_ string, property string) (string, error) {
				showCalls++
				switch showCalls {
				case 1:
					if property != "InvocationID" {
						t.Fatalf("first property = %q, want InvocationID", property)
					}
					return "original", nil
				case 2:
					if property != "ActiveState" {
						t.Fatalf("post-stop property = %q, want ActiveState", property)
					}
					return state, nil
				default:
					t.Fatalf("unexpected systemd property read %q", property)
					return "", nil
				}
			}
			systemdStopUnit = func(string) error { return nil }

			if err := (systemdUserScopes{}).stop(ownedScope{unit: "gascity-pane-0123456789abcdef0123456789abcdef.scope", invocationID: "original"}); err != nil {
				t.Fatalf("stop retained %s scope: %v", state, err)
			}
		})
	}
}

func TestSystemdUserScopesStopRetainsCommandFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd user scopes require Linux")
	}
	oldShow, oldStop := systemdShowProperty, systemdStopUnit
	t.Cleanup(func() {
		systemdShowProperty, systemdStopUnit = oldShow, oldStop
	})
	systemdShowProperty = func(string, string) (string, error) { return "original", nil }
	systemdStopUnit = func(string) error { return errors.New("access denied") }

	err := (systemdUserScopes{}).stop(ownedScope{unit: "gascity-pane-0123456789abcdef0123456789abcdef.scope", invocationID: "original"})
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("stop error = %v, want command failure", err)
	}
}

func TestSystemdUserScopesStopRejectsForeignScopeBeforeProbe(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd user scopes require Linux")
	}
	oldShow, oldStop := systemdShowProperty, systemdStopUnit
	t.Cleanup(func() {
		systemdShowProperty, systemdStopUnit = oldShow, oldStop
	})
	systemdShowProperty = func(string, string) (string, error) {
		t.Fatal("systemd show called for a scope outside the dedicated pane namespace")
		return "", nil
	}
	systemdStopUnit = func(string) error {
		t.Fatal("systemd stop called for a scope outside the dedicated pane namespace")
		return nil
	}

	err := (systemdUserScopes{}).stop(ownedScope{unit: "tmux-spawn-123.scope", invocationID: "original"})
	if err == nil || !strings.Contains(err.Error(), "dedicated Gas City pane scope") {
		t.Fatalf("stop error = %v, want dedicated pane namespace rejection", err)
	}
}

type fakeOwnedScopes struct {
	captured ownedScope
	stopped  ownedScope
	stops    []ownedScope
	err      error
}

func (f *fakeOwnedScopes) capture(unit string) (ownedScope, error) {
	if f.captured == (ownedScope{}) && f.err == nil {
		return ownedScope{unit: unit, invocationID: "invocation"}, nil
	}
	return f.captured, f.err
}

func (f *fakeOwnedScopes) stop(scope ownedScope) error {
	f.stopped = scope
	f.stops = append(f.stops, scope)
	return f.err
}

func TestOwnedScopeStopRejectsChangedSessionIncarnation(t *testing.T) {
	// A replacement session may reuse the tmux name. Its current instance token
	// must match the token recorded with the scope before systemd is contacted.
	fs := &fakeOwnedScopes{}
	tm := NewTmux()
	tm.ownedScopes = fs
	tm.exec = &fakeExecutor{outs: []string{
		ownedScopeEnv + "=scope",
		ownedScopeInvocationEnv + "=invocation",
		ownedScopeTokenEnv + "=old-token",
		"GC_INSTANCE_TOKEN=new-token",
	}}
	tm.stopOwnedScope("managed")
	if fs.stopped != (ownedScope{}) {
		t.Fatalf("stopped = %+v, want no stop for a replaced session", fs.stopped)
	}
}

func TestOwnedScopeStopUsesRecordedInvocation(t *testing.T) {
	fs := &fakeOwnedScopes{}
	tm := NewTmux()
	tm.ownedScopes = fs
	tm.exec = &fakeExecutor{outs: []string{
		ownedScopeEnv + "=gascity-pane-0123456789abcdef0123456789abcdef.scope",
		ownedScopeInvocationEnv + "=invocation",
		ownedScopeTokenEnv + "=token",
		"GC_INSTANCE_TOKEN=token",
	}}
	tm.stopOwnedScope("managed")
	if got, want := fs.stopped, (ownedScope{unit: "gascity-pane-0123456789abcdef0123456789abcdef.scope", invocationID: "invocation"}); got != want {
		t.Fatalf("stopped = %+v, want %+v", got, want)
	}
}

func TestOwnedScopeStopRetainsLegacyOrMalformedScopeWithoutStopping(t *testing.T) {
	for _, unit := range []string{
		"tmux-spawn-123.scope",
		"gascity-pane-0123456789abcdef0123456789abcdef.service",
		"gascity-pane-0123456789ABCDEF0123456789abcdef.scope",
		"gascity-pane-0123456789abcdef0123456789abcde.scope",
	} {
		t.Run(unit, func(t *testing.T) {
			fs := &fakeOwnedScopes{}
			tm := NewTmux()
			tm.ownedScopes = fs
			tm.exec = &fakeExecutor{outs: []string{
				ownedScopeEnv + "=" + unit,
				ownedScopeInvocationEnv + "=invocation",
				ownedScopeTokenEnv + "=token",
				"GC_INSTANCE_TOKEN=token",
			}}

			tm.stopOwnedScope("managed")
			if fs.stopped != (ownedScope{}) {
				t.Fatalf("stopped = %+v, want legacy metadata retained without stop", fs.stopped)
			}
		})
	}
}

func TestOwnedScopeStopFailureRetainsScope(t *testing.T) {
	fs := &fakeOwnedScopes{err: errors.New("scope gone")}
	tm := NewTmux()
	tm.ownedScopes = fs
	tm.exec = &fakeExecutor{outs: []string{
		ownedScopeEnv + "=gascity-pane-0123456789abcdef0123456789abcdef.scope",
		ownedScopeInvocationEnv + "=invocation",
		ownedScopeTokenEnv + "=token",
		"GC_INSTANCE_TOKEN=token",
	}}
	tm.stopOwnedScope("managed")
	if fs.stopped.unit != "gascity-pane-0123456789abcdef0123456789abcdef.scope" {
		t.Fatalf("stop did not receive witnessed scope: %+v", fs.stopped)
	}
}
