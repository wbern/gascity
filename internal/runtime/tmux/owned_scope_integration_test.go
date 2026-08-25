//go:build integration

package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/shellquote"
)

func TestOwnedPaneScopeStopsOnlyRecordedDescendants(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd user scopes require Linux")
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("systemd-run is not installed")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("systemctl is not installed")
	}
	if err := exec.Command("systemctl", "--user", "is-system-running").Run(); err != nil {
		t.Skipf("systemd user manager is unavailable: %v", err)
	}
	if !hasTmux() {
		t.Skip("tmux is not installed")
	}

	t.Setenv(AgentSliceEnv, fmt.Sprintf("gascity-owned-scope-%d.slice", time.Now().UnixNano()))
	tm := testTmux()
	t.Cleanup(func() { _ = tm.KillServer() })

	start := func(name string) (string, ownedScope) {
		t.Helper()
		pidFile := filepath.Join(t.TempDir(), name+".pid")
		command := fmt.Sprintf("sleep 600 & printf '%%s' \"$!\" > %s; wait", shellquote.Quote(pidFile))
		if err := tm.NewSessionWithCommandAndEnv(name, "", command, map[string]string{"GC_INSTANCE_TOKEN": name + "-token"}); err != nil {
			t.Fatalf("start %q: %v", name, err)
		}
		var pid string
		waitForOwnedScope(t, func() bool {
			b, err := os.ReadFile(pidFile)
			if err != nil {
				return false
			}
			pid = strings.TrimSpace(string(b))
			unit, unitErr := tm.GetEnvironment(name, ownedScopeEnv)
			invocationID, invocationErr := tm.GetEnvironment(name, ownedScopeInvocationEnv)
			return unitErr == nil && invocationErr == nil && isGasCityPaneScope(unit) && invocationID != ""
		})
		unit, err := tm.GetEnvironment(name, ownedScopeEnv)
		if err != nil {
			t.Fatalf("read %q scope unit: %v", name, err)
		}
		invocationID, err := tm.GetEnvironment(name, ownedScopeInvocationEnv)
		if err != nil {
			t.Fatalf("read %q invocation: %v", name, err)
		}
		return pid, ownedScope{unit: unit, invocationID: invocationID}
	}

	targetName := fmt.Sprintf("owned-target-%d", time.Now().UnixNano())
	siblingName := fmt.Sprintf("owned-sibling-%d", time.Now().UnixNano())
	targetPID, targetScope := start(targetName)
	_, siblingScope := start(siblingName)
	if targetScope.unit == siblingScope.unit {
		t.Fatalf("target and sibling shared scope %q", targetScope.unit)
	}
	if err := tm.KillSessionWithProcesses(targetName); err != nil {
		t.Fatalf("kill target session: %v", err)
	}
	waitForOwnedScope(t, func() bool {
		pid, err := strconv.Atoi(targetPID)
		return err == nil && syscall.Kill(pid, 0) == syscall.ESRCH
	})
	if err := exec.Command("systemctl", "--user", "is-active", "--quiet", siblingScope.unit).Run(); err != nil {
		t.Fatalf("sibling scope %q was not preserved: %v", siblingScope.unit, err)
	}
	_ = tm.KillSessionWithProcesses(siblingName)
}

func waitForOwnedScope(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("timed out waiting for owned pane scope")
}
