package tmux

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
)

const (
	ownedScopeEnv           = "GC_TMUX_OWNED_SCOPE"
	ownedScopeInvocationEnv = "GC_TMUX_OWNED_SCOPE_INVOCATION"
	ownedScopeTokenEnv      = "GC_TMUX_OWNED_SCOPE_TOKEN"
)

// ownedScope is a systemd transient scope witnessed while tmux materialized a
// pane. InvocationID fences a unit name against a later unit reuse.
type ownedScope struct {
	unit         string
	invocationID string
}

// scopeLifecycle isolates the systemd boundary so ownership decisions remain
// fast and deterministic in unit tests.
type scopeLifecycle interface {
	capture(pid string) (ownedScope, error)
	stop(scope ownedScope) error
}

type systemdUserScopes struct{}

func (systemdUserScopes) capture(pid string) (ownedScope, error) {
	if runtime.GOOS != "linux" {
		return ownedScope{}, fmt.Errorf("systemd user scopes unsupported on %s", runtime.GOOS)
	}
	b, err := os.ReadFile("/proc/" + pid + "/cgroup")
	if err != nil {
		return ownedScope{}, fmt.Errorf("reading pane cgroup: %w", err)
	}
	unit := tmuxSpawnScope(string(b))
	if unit == "" {
		return ownedScope{}, fmt.Errorf("pane is not in a tmux-spawn scope")
	}
	invocationID, err := systemdShow(unit, "InvocationID")
	if err != nil {
		return ownedScope{}, fmt.Errorf("reading scope %q invocation identity: %w", unit, err)
	}
	if invocationID == "" {
		return ownedScope{}, fmt.Errorf("scope %q has no invocation identity", unit)
	}
	return ownedScope{unit: unit, invocationID: invocationID}, nil
}

func (systemdUserScopes) stop(scope ownedScope) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("systemd user scopes unsupported on %s", runtime.GOOS)
	}
	if scope.unit == "" || scope.invocationID == "" {
		return fmt.Errorf("incomplete scope ownership record")
	}
	liveID, err := systemdShow(scope.unit, "InvocationID")
	if err != nil {
		if systemdUnitGone(err) {
			return nil
		}
		return fmt.Errorf("reading scope %q before stop: %w", scope.unit, err)
	}
	if liveID != scope.invocationID {
		return fmt.Errorf("scope %q invocation changed", scope.unit)
	}
	cmd := exec.CommandContext(context.Background(), "systemctl", "--user", "stop", scope.unit)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("stopping scope %q: %w: %s", scope.unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func systemdUnitGone(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "not loaded")
}

func systemdShow(unit, property string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "systemctl", "--user", "show", "--value", "--property="+property, unit)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("systemctl show: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func tmuxSpawnScope(cgroup string) string {
	s := bufio.NewScanner(strings.NewReader(cgroup))
	for s.Scan() {
		fields := strings.SplitN(s.Text(), ":", 3)
		if len(fields) != 3 {
			continue
		}
		unit := path.Base(fields[2])
		if strings.HasPrefix(unit, "tmux-spawn-") && strings.HasSuffix(unit, ".scope") {
			return unit
		}
	}
	return ""
}
