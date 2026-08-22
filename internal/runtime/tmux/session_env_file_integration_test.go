//go:build integration

package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSecretEnvIsAbsentFromProcessTable is deliberately process-table based so
// it works on Darwin as well as Linux. It retains an inert control: a scan that
// cannot see ordinary -e environment transport is not evidence that secrets
// are absent. The helper returns only booleans; command lines are never logged.
func TestSecretEnvIsAbsentFromProcessTable(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	stamp := time.Now().UnixNano()
	control := fmt.Sprintf("gc-argv-control-%d", stamp)
	secret := fmt.Sprintf("gc-argv-secret-%d", stamp)
	newTmux := func(kind string) *Tmux {
		cfg := DefaultConfig()
		cfg.SocketName = fmt.Sprintf("gctest-argv-%s-%d", kind, stamp)
		return NewTmuxWithConfig(cfg)
	}
	controlTM, secretTM := newTmux("control"), newTmux("secret")
	controlName, secretName := fmt.Sprintf("gcargvctl-%d", stamp), fmt.Sprintf("gcargvsec-%d", stamp)
	t.Cleanup(func() { _ = controlTM.KillSession(controlName); _ = secretTM.KillSession(secretName) })
	if err := controlTM.NewSessionWithCommandAndEnv(controlName, "", "sleep 30", map[string]string{"GC_RIG": control}); err != nil {
		t.Fatalf("control session: %v", err)
	}
	if err := secretTM.NewSessionWithCommandAndEnv(secretName, "", "sleep 30", map[string]string{"OPENAI_API_KEY": secret}); err != nil {
		t.Fatalf("secret session: %v", err)
	}
	if !processTableContains(control) {
		t.Fatal("inert argv control was not visible in the process table")
	}
	if processTableContains(secret) {
		t.Fatal("secret canary was visible in the process table")
	}
	if got, err := secretTM.GetEnvironment(secretName, "OPENAI_API_KEY"); err != nil || got != secret {
		t.Fatalf("session readback was not byte-identical")
	}
	out := filepath.Join(t.TempDir(), "pane-env")
	command := "sh -c 'printf %s \"$OPENAI_API_KEY\" > \"$1\"' sh " + out
	if _, err := secretTM.run("new-window", "-d", "-t", secretName, command); err != nil {
		t.Fatalf("new-window: %v", err)
	}
	waitForMarker(t, out, secret)
	if leftovers := stagedCommandDirectories(t); len(leftovers) != 0 {
		t.Fatal("secret staging directory survived session creation")
	}
}

// TestSecretEnvIsAbsentFromProcCmdline retains the upstream Linux-specific
// /proc control separately from the portable Darwin process-table test above.
// It returns only boolean matches, never process arguments.
func TestSecretEnvIsAbsentFromProcCmdline(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("no /proc cmdline")
	}
	stamp := time.Now().UnixNano()
	control := fmt.Sprintf("gc-proc-control-%d", stamp)
	secret := fmt.Sprintf("gc-proc-secret-%d", stamp)
	newTmux := func(kind string) *Tmux {
		cfg := DefaultConfig()
		cfg.SocketName = fmt.Sprintf("gctest-proc-%s-%d", kind, stamp)
		return NewTmuxWithConfig(cfg)
	}
	controlTM, secretTM := newTmux("control"), newTmux("secret")
	controlName, secretName := fmt.Sprintf("gcprocctl-%d", stamp), fmt.Sprintf("gcprocsec-%d", stamp)
	t.Cleanup(func() { _ = controlTM.KillSession(controlName); _ = secretTM.KillSession(secretName) })
	if err := controlTM.NewSessionWithCommandAndEnv(controlName, "", "sleep 30", map[string]string{"GC_RIG": control}); err != nil {
		t.Fatalf("control session: %v", err)
	}
	if err := secretTM.NewSessionWithCommandAndEnv(secretName, "", "sleep 30", map[string]string{"OPENAI_API_KEY": secret}); err != nil {
		t.Fatalf("secret session: %v", err)
	}
	if !procCmdlineContains(control) {
		t.Fatal("inert argv control was not visible in /proc cmdline")
	}
	if procCmdlineContains(secret) {
		t.Fatal("secret canary was visible in /proc cmdline")
	}
}

func procCmdlineContains(needle string) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err == nil && strings.Contains(string(body), needle) {
			return true
		}
	}
	return false
}

func processTableContains(needle string) bool {
	output, err := exec.Command("ps", "-axww", "-o", "command=").Output()
	return err == nil && strings.Contains(string(output), needle)
}

func stagedCommandDirectories(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("read temporary directory: %v", err)
	}
	var found []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), stagedDirPrefix) {
			found = append(found, entry.Name())
		}
	}
	return found
}
