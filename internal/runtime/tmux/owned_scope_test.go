package tmux

import (
	"errors"
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

func TestSystemdUserScopesStopRetainsCommandFailure(t *testing.T) {
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

type fakeOwnedScopes struct {
	captured ownedScope
	stopped  ownedScope
	err      error
}

func (f *fakeOwnedScopes) capture(unit string) (ownedScope, error) {
	if f.captured == (ownedScope{}) && f.err == nil {
		return ownedScope{unit: unit, invocationID: "invocation"}, nil
	}
	return f.captured, f.err
}
func (f *fakeOwnedScopes) stop(scope ownedScope) error { f.stopped = scope; return f.err }

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
		ownedScopeEnv + "=scope",
		ownedScopeInvocationEnv + "=invocation",
		ownedScopeTokenEnv + "=token",
		"GC_INSTANCE_TOKEN=token",
	}}
	tm.stopOwnedScope("managed")
	if got, want := fs.stopped, (ownedScope{unit: "scope", invocationID: "invocation"}); got != want {
		t.Fatalf("stopped = %+v, want %+v", got, want)
	}
}

func TestOwnedScopeStopFailureRetainsScope(t *testing.T) {
	fs := &fakeOwnedScopes{err: errors.New("scope gone")}
	tm := NewTmux()
	tm.ownedScopes = fs
	tm.exec = &fakeExecutor{outs: []string{
		ownedScopeEnv + "=scope",
		ownedScopeInvocationEnv + "=invocation",
		ownedScopeTokenEnv + "=token",
		"GC_INSTANCE_TOKEN=token",
	}}
	tm.stopOwnedScope("managed")
	if fs.stopped.unit != "scope" {
		t.Fatalf("stop did not receive witnessed scope: %+v", fs.stopped)
	}
}
