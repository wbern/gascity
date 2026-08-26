package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
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
	capture(unit string) (ownedScope, error)
	stop(scope ownedScope) error
}

type systemdUserScopes struct{}

const (
	ownedScopeCaptureTimeout  = time.Second
	ownedScopeCaptureInterval = 25 * time.Millisecond
)

var systemdShowProperty = systemdShow

var systemdStopUnit = func(unit string) error {
	cmd := exec.CommandContext(context.Background(), "systemctl", "--user", "stop", unit)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (systemdUserScopes) capture(unit string) (ownedScope, error) {
	if runtime.GOOS != "linux" {
		return ownedScope{}, fmt.Errorf("systemd user scopes unsupported on %s", runtime.GOOS)
	}
	if !isGasCityPaneScope(unit) {
		return ownedScope{}, fmt.Errorf("pane is not in a dedicated Gas City pane scope")
	}
	deadline := time.Now().Add(ownedScopeCaptureTimeout)
	for {
		invocationID, err := systemdShowProperty(unit, "InvocationID")
		if err == nil && invocationID != "" {
			return ownedScope{unit: unit, invocationID: invocationID}, nil
		}
		if err != nil && !systemdUnitGone(err) {
			return ownedScope{}, fmt.Errorf("reading scope %q invocation identity: %w", unit, err)
		}
		if time.Now().After(deadline) {
			if err != nil {
				return ownedScope{}, fmt.Errorf("reading scope %q invocation identity: %w", unit, err)
			}
			return ownedScope{}, fmt.Errorf("scope %q has no invocation identity", unit)
		}
		time.Sleep(ownedScopeCaptureInterval)
	}
}

func (systemdUserScopes) stop(scope ownedScope) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("systemd user scopes unsupported on %s", runtime.GOOS)
	}
	if !isGasCityPaneScope(scope.unit) {
		return fmt.Errorf("scope %q is not in the dedicated Gas City pane scope namespace", scope.unit)
	}
	if scope.unit == "" || scope.invocationID == "" {
		return fmt.Errorf("incomplete scope ownership record")
	}
	liveID, err := systemdShowProperty(scope.unit, "InvocationID")
	if err != nil {
		if systemdUnitGone(err) {
			return nil
		}
		return fmt.Errorf("reading scope %q before stop: %w", scope.unit, err)
	}
	if liveID != scope.invocationID {
		return fmt.Errorf("scope %q invocation changed", scope.unit)
	}
	if err := systemdStopUnit(scope.unit); err != nil {
		return fmt.Errorf("stopping scope %q: %w", scope.unit, err)
	}
	activeState, err := systemdShowProperty(scope.unit, "ActiveState")
	if err != nil {
		if systemdUnitGone(err) {
			return nil
		}
		return fmt.Errorf("verifying stopped scope %q: %w", scope.unit, err)
	}
	switch strings.TrimSpace(activeState) {
	case "inactive", "failed":
		return nil
	default:
		return fmt.Errorf("scope %q remained active after stop (ActiveState=%q)", scope.unit, activeState)
	}
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
