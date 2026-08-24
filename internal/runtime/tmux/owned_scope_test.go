package tmux

import (
	"errors"
	"testing"
)

func TestTmuxSpawnScope(t *testing.T) {
	if got := tmuxSpawnScope("0::/user.slice/user-1000.slice/user@1000.service/app.slice/tmux-spawn-8123.scope\n"); got != "tmux-spawn-8123.scope" {
		t.Fatalf("tmuxSpawnScope = %q", got)
	}
	if got := tmuxSpawnScope("0::/user.slice/user-1000.slice/user@1000.service/app.slice/other.scope\n"); got != "" {
		t.Fatalf("tmuxSpawnScope unrelated = %q, want empty", got)
	}
}

func TestSystemdUnitGone(t *testing.T) {
	if !systemdUnitGone(errors.New("systemctl show: Unit tmux-spawn-1.scope not found")) {
		t.Fatal("already-gone systemd scope was not treated as idempotent")
	}
	if systemdUnitGone(errors.New("permission denied")) {
		t.Fatal("command failure must retain the scope for diagnostics")
	}
}

type fakeOwnedScopes struct {
	captured ownedScope
	stopped  ownedScope
	err      error
}

func (f *fakeOwnedScopes) capture(string) (ownedScope, error) { return f.captured, f.err }
func (f *fakeOwnedScopes) stop(scope ownedScope) error        { f.stopped = scope; return f.err }

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
