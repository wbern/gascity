//go:build unix

package dolt_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A Dolt server started from an agent session is a descendant of that session
// and dies when the session is stopped (2026-11-22: a session's
// `gc dolt restart --force` took the whole fleet's bead store down). gc dolt
// start/restart/recover therefore refuse inside an agent session
// (GC_SESSION_ID), unless an operator opts in with GC_DOLT_ALLOW_SESSION_CONTROL=1.

func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("not an exit error: %v", err)
	return -1
}

func TestRestartRefusesInsideAgentSession(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()
	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 0, "start": 0})
	portEnv := fmt.Sprintf("GC_DOLT_PORT=%d", port)

	for _, args := range [][]string{nil, {"--force"}} {
		out, err := runRestartWithEnv(t, cityPath, root, []string{portEnv, "GC_SESSION_ID=gc2-test1"}, args...)
		if got := exitCode(t, err); got != 77 {
			t.Fatalf("args=%v: exit=%d, want 77\n%s", args, got, out)
		}
		for _, want := range []string{"refusing to run inside agent session gc2-test1", "gc-restart", "GC_DOLT_ALLOW_SESSION_CONTROL=1"} {
			if !strings.Contains(string(out), want) {
				t.Fatalf("args=%v: output lacks %q:\n%s", args, want, out)
			}
		}
		if data, err := os.ReadFile(bdLog); err == nil && strings.TrimSpace(string(data)) != "" {
			t.Fatalf("args=%v: gc-beads-bd was invoked despite the refusal:\n%s", args, data)
		}
	}
}

func TestRestartProceedsInsideAgentSessionWithOperatorOverride(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()
	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 0, "start": 0})
	out, err := runRestartWithEnv(t, cityPath, root, []string{
		fmt.Sprintf("GC_DOLT_PORT=%d", port), "GC_SESSION_ID=gc2-test1", "GC_DOLT_ALLOW_SESSION_CONTROL=1",
	})
	if err != nil {
		t.Fatalf("override should let restart run: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(bdLog)
	if got := strings.Join(strings.Fields(string(data)), " "); got != "stop start" {
		t.Fatalf("ops = %q, want \"stop start\"\n%s", got, out)
	}
}

func TestRestartIsUnaffectedOutsideAgentSessions(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()
	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 0, "start": 0})
	// No GC_SESSION_ID (filteredEnv strips every GC_* variable first).
	if out, err := runRestart(t, cityPath, root, port); err != nil {
		t.Fatalf("restart outside a session must behave as before: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(bdLog)
	if got := strings.Join(strings.Fields(string(data)), " "); got != "stop start" {
		t.Fatalf("ops = %q, want \"stop start\"", got)
	}
}

func TestStartAndRecoverRefuseInsideAgentSession(t *testing.T) {
	root := repoRoot(t)
	for _, cmd := range []string{"start", "recover"} {
		t.Run(cmd, func(t *testing.T) {
			cityPath := t.TempDir()
			bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{})
			script := filepath.Join(root, "commands", cmd, "run.sh")
			run := func(extra ...string) (string, int) {
				c := exec.Command("sh", script)
				c.Env = append(filteredEnv("PATH"),
					"PATH="+os.Getenv("PATH"), "GC_CITY_PATH="+cityPath, "GC_PACK_DIR="+root,
					"GC_DOLT_USER=root", "GC_DOLT_PASSWORD=",
					"GC_BEADS_BD_SCRIPT="+filepath.Join(cityPath, ".gc", "scripts", "gc-beads-bd.sh"),
				)
				c.Env = append(c.Env, extra...)
				out, err := c.CombinedOutput()
				return string(out), exitCode(t, err)
			}

			out, code := run("GC_SESSION_ID=gc2-test1")
			if code != 77 || !strings.Contains(out, "refusing to run inside agent session gc2-test1") {
				t.Fatalf("gc dolt %s inside a session: exit=%d, want 77 with a refusal\n%s", cmd, code, out)
			}
			if data, err := os.ReadFile(bdLog); err == nil && strings.TrimSpace(string(data)) != "" {
				t.Fatalf("gc dolt %s reached gc-beads-bd despite the refusal:\n%s", cmd, data)
			}

			out, _ = run("GC_SESSION_ID=gc2-test1", "GC_DOLT_ALLOW_SESSION_CONTROL=1")
			if strings.Contains(out, "refusing to run inside agent session") {
				t.Fatalf("override ignored for gc dolt %s:\n%s", cmd, out)
			}
		})
	}
}

// TestSessionGuardFailsOpenWhenItsFileIsMissing: a broken install must not make
// a legitimate restart impossible.
func TestSessionGuardFailsOpenWhenItsFileIsMissing(t *testing.T) {
	src := repoRoot(t)
	packCopy := t.TempDir()
	if out, err := exec.Command("cp", "-R", src+"/.", packCopy).CombinedOutput(); err != nil {
		t.Fatalf("copy pack: %v\n%s", err, out)
	}
	if err := os.Remove(filepath.Join(packCopy, "assets", "scripts", "session_guard.sh")); err != nil {
		t.Fatalf("remove guard: %v", err)
	}
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()
	cityPath := t.TempDir()
	writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 0, "start": 0})
	out, err := runRestartWithEnv(t, cityPath, packCopy, []string{fmt.Sprintf("GC_DOLT_PORT=%d", port), "GC_SESSION_ID=gc2-test1"})
	if strings.Contains(string(out), "refusing to run inside agent session") {
		t.Fatalf("a missing guard file must fail open, got a refusal:\n%s", out)
	}
	if err != nil {
		t.Fatalf("restart should have run with the guard file missing: %v\n%s", err, out)
	}
}
